//go:build linux

package nodebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"yagp/pkg/nodegrpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Serialize nftables updates and the reads and persistence they depend on.
var nftApplyMu sync.Mutex

type nftObject map[string]json.RawMessage
type nftCommand map[string]map[string]nftObject

type nftDocument struct {
	Commands []nftCommand `json:"nftables"`
}

func (s *controlServer) GetNftConfig(ctx context.Context, _ *emptypb.Empty) (*nodegrpc.NftConfig, error) {
	nft, err := exec.LookPath("nft")
	if err != nil {
		return nil, status.Error(codes.Unimplemented, "nftables is unavailable: install the nftables package on this node")
	}
	table, legacy := nftTables(s.config.NodeID, s.config.NftConfigFile)
	quota, legacyQuota := trafficQuotaTables(s.config)
	redirect := nodegrpc.TransparentRedirectTableName(s.config.NodeID)
	config, err := readLiveNftConfig(ctx, nft, []string{table, legacy, quota, legacyQuota, redirect})
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Errorf(codes.FailedPrecondition, "read nftables rules: %v", err)
	}
	// Older panels expect the saved add commands in Config.
	saved, _ := readNftConfig(s.config.NftConfigFile)
	return &nodegrpc.NftConfig{Config: renameNftConfig(saved, legacy, table), Table: table, LiveConfig: config}, nil
}

func readLiveNftConfig(ctx context.Context, nft string, managedTables []string) (string, error) {
	nftApplyMu.Lock()
	defer nftApplyMu.Unlock()
	output, err := nftOutput(ctx, nft, nil, "-j", "list", "tables")
	if err != nil {
		return "", err
	}
	var tables struct {
		Objects []struct {
			Table struct {
				Family string `json:"family"`
				Name   string `json:"name"`
			} `json:"table"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(output, &tables); err != nil {
		return "", err
	}

	type listing struct {
		Objects []nftObject `json:"nftables"`
	}
	result := listing{Objects: []nftObject{}}
	for _, object := range tables.Objects {
		table := object.Table
		if table.Family != "inet" || !slices.Contains(managedTables, table.Name) {
			continue
		}
		output, err = nftOutput(ctx, nft, nil, "-j", "list", "table", table.Family, table.Name)
		if err != nil {
			return "", err
		}
		var rules listing
		if err := json.Unmarshal(output, &rules); err != nil {
			return "", err
		}
		for _, rule := range rules.Objects {
			if _, metadata := rule["metainfo"]; !metadata {
				result.Objects = append(result.Objects, rule)
			}
		}
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	return string(encoded), err
}

func (s *controlServer) ApplyNftConfig(ctx context.Context, req *nodegrpc.NftConfig) (*nodegrpc.NftConfig, error) {
	if err := applyNftConfig(ctx, s.config.NodeID, s.config.NftConfigFile, req.GetConfig()); err != nil {
		return nil, err
	}
	return s.nftConfigReply(req.GetConfig()), nil
}

func (s *controlServer) nftConfigReply(config string) *nodegrpc.NftConfig {
	table, legacy := nftTables(s.config.NodeID, s.config.NftConfigFile)
	return &nodegrpc.NftConfig{Config: renameNftConfig(config, legacy, table), Table: table}
}

func renameNftConfig(config, from, to string) string {
	if from != "" && config != "" {
		if doc, err := parseNftConfig(config); err == nil && doc.renameTable(from, to) {
			if renamed, err := json.MarshalIndent(doc, "", "  "); err == nil {
				return string(renamed)
			}
		}
	}
	return config
}

// ReadNftTemplate uses the snapshot fetched by the daemon's established session.
func ReadNftTemplate(configPath string) (string, error) {
	config, err := readPanelConfig(configPath)
	if err != nil {
		return "", err
	}
	if config.GetNftTemplate() == "" {
		return "", errors.New("cached node configuration has no nftables template")
	}
	return config.GetNftTemplate(), nil
}

func ReadNodeNftTemplate(configPath string, id uint64) (string, error) {
	template, err := ReadNftTemplate(configPath)
	table, legacy := nftTables(id, configPath)
	return renameNftConfig(template, legacy, table), err
}

func readNftConfig(configPath string) (string, error) {
	config, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(config), err
}

func restoreNftConfig(ctx context.Context, id uint64, configPath string) error {
	config, err := readNftConfig(configPath)
	if err != nil || config == "" {
		return err
	}
	return updateNft(ctx, id, configPath, config, false)
}

func applyNftConfig(ctx context.Context, id uint64, configPath, config string) error {
	return updateNft(ctx, id, configPath, config, true)
}

func updateNft(ctx context.Context, id uint64, configPath, config string, persist bool) error {
	if configPath == "" {
		return status.Error(codes.Unimplemented, "nftables management is unavailable on this node")
	}
	table, legacy := nftTables(id, configPath)
	batch, err := nftBatch(table, legacy, config)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	nft, err := exec.LookPath("nft")
	if err != nil {
		return status.Error(codes.Unimplemented, "nftables is unavailable: install the nftables package on this node")
	}
	nftApplyMu.Lock()
	defer nftApplyMu.Unlock()
	if err = checkNftBatch(ctx, nft, batch); err != nil {
		return err
	}
	var staged string
	if persist {
		staged, err = stageNftConfig(configPath, config)
		if err != nil {
			return status.Errorf(codes.Internal, "stage nftables config: %v", err)
		}
		defer os.Remove(staged)
	}
	if err = runNft(ctx, nft, batch, "-j", "-f", "-"); err != nil {
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		return status.Errorf(codes.FailedPrecondition, "apply nftables config: %v", err)
	}
	if persist {
		if err = os.Rename(staged, configPath); err != nil {
			return status.Errorf(codes.Internal, "nftables rules applied, but could not save the config: %v", err)
		}
	}
	return nil
}

func nftTables(id uint64, configPath string) (table, legacy string) {
	table = nodegrpc.NftTableName(id)
	if configPath != "" && ReadRuntimeLayout(configPath) >= singleNodeLayoutVersion {
		return nodegrpc.SingleNodeNftTable, table
	}
	return table, ""
}

func parseNftConfig(config string) (nftDocument, error) {
	var document nftDocument
	if len(config) > nodegrpc.MaxNftConfigBytes {
		return document, fmt.Errorf("nftables config exceeds %d bytes", nodegrpc.MaxNftConfigBytes)
	}
	var trailing json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(config))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil || document.Commands == nil || decoder.Decode(&trailing) != io.EOF {
		return document, errors.New("expected one JSON object with an nftables command array")
	}
	return document, nil
}

func (d nftDocument) renameTable(from, to string) bool {
	if from == "" {
		return false
	}
	changed := false
	for _, command := range d.Commands {
		for _, object := range command["add"] {
			var table string
			if json.Unmarshal(object["table"], &table) == nil && table == from {
				object["table"] = json.RawMessage(strconv.Quote(to))
				changed = true
			}
		}
	}
	return changed
}

func nftBatch(table, legacy, config string) ([]byte, error) {
	document, err := parseNftConfig(config)
	if err != nil {
		return nil, err
	}
	document.renameTable(legacy, table)
	for i, command := range document.Commands {
		if err := validateNftCommand(command, table); err != nil {
			return nil, fmt.Errorf("nftables command %d: %w", i+1, err)
		}
	}
	// Add is idempotent. Add then delete handles absent tables on versions without destroy.
	var batch []nftCommand
	if legacy != "" {
		batch = append(batch, nftTableCommand("add", legacy), nftTableCommand("delete", legacy))
	}
	addTable := nftTableCommand("add", table)
	batch = append(batch, addTable, nftTableCommand("delete", table))
	if len(document.Commands) > 0 {
		batch = append(batch, addTable)
		batch = append(batch, document.Commands...)
	}
	return json.Marshal(nftDocument{Commands: batch})
}

func nftTableCommand(action, table string) nftCommand {
	identity := nftObject{"family": json.RawMessage(`"inet"`), "name": json.RawMessage(strconv.Quote(table))}
	return nftCommand{action: {"table": identity}}
}

func validateNftCommand(command nftCommand, table string) error {
	objects := command["add"]
	if len(command) != 1 || len(objects) != 1 {
		return errors.New("expected one add command")
	}
	for kind, object := range objects {
		switch kind {
		case "chain", "rule", "set", "map", "element", "counter", "quota", "limit", "flowtable", "ct helper", "ct timeout", "ct expectation", "synproxy":
		default:
			return fmt.Errorf("cannot add a %s object", kind)
		}
		var family, name string
		if json.Unmarshal(object["family"], &family) != nil || json.Unmarshal(object["table"], &name) != nil ||
			family != "inet" || name != table {
			return fmt.Errorf("objects must target table inet %s", table)
		}
	}
	return nil
}

func checkNftBatch(ctx context.Context, nft string, batch []byte) error {
	err := runNft(ctx, nft, batch, "-c", "-j", "-f", "-")
	if err == nil {
		return nil
	}
	// A failing check can also mean the kernel or permissions are unavailable.
	probe := runNft(ctx, nft, nil, "-j", "list", "tables")
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if probe != nil {
		return status.Errorf(codes.FailedPrecondition, "cannot access nftables: %v", probe)
	}
	return status.Errorf(codes.InvalidArgument, "nftables check failed: %v", err)
}

func runNft(ctx context.Context, nft string, batch []byte, args ...string) error {
	_, err := nftOutput(ctx, nft, batch, args...)
	return err
}

func nftOutput(ctx context.Context, nft string, batch []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, nft, args...)
	cmd.Stdin = bytes.NewReader(batch)
	output, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		detail := strings.TrimSpace(string(exit.Stderr))
		if len(detail) > 2048 {
			detail = strings.ToValidUTF8(detail[:2048], "�") + "…"
		}
		if detail != "" {
			return nil, errors.New(detail)
		}
	}
	return output, err
}

func stageNftConfig(path, config string) (string, error) {
	info, err := os.Lstat(path)
	if err == nil && !info.Mode().IsRegular() {
		return "", errors.New("nftables config path must be a regular file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".nft-config-*")
	if err != nil {
		return "", err
	}
	_, err = file.WriteString(config)
	if err = errors.Join(err, file.Close()); err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}
