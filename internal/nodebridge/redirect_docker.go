//go:build linux

package nodebridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jsimonetti/rtnetlink"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

const dockerSocketPath = "/var/run/docker.sock"

type dockerBridge struct {
	Name    string
	Address netip.Addr
}

func localDockerClient(socket string) *http.Client {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 2 * time.Second,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func localDockerBridgeNames(ctx context.Context, socket string) map[string]struct{} {
	client := localDockerClient(socket)
	client.Timeout = 2 * time.Second
	// Use the local socket regardless of CLI settings or DOCKER_HOST.
	query := url.Values{"filters": {`{"driver":["bridge"]}`}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/networks?"+query.Encode(), nil)
	if err != nil {
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil
	}
	var networks []struct {
		ID      string
		Name    string
		Driver  string
		Options map[string]string
	}
	if err := json.NewDecoder(response.Body).Decode(&networks); err != nil {
		return nil
	}
	names := make(map[string]struct{})
	for _, network := range networks {
		if network.Driver != "bridge" {
			continue
		}
		name := network.Options["com.docker.network.bridge.name"]
		if name == "" && network.Name == "bridge" {
			name = "docker0"
		}
		if name == "" && len(network.ID) >= 12 {
			candidate := "br-" + network.ID[:12]
			if conventionalDockerBridge(candidate) {
				name = candidate
			}
		}
		if name != "" {
			names[name] = struct{}{}
		}
	}
	return names
}

func conventionalDockerBridge(name string) bool {
	if name == "docker0" {
		return true
	}
	id, ok := strings.CutPrefix(name, "br-")
	if !ok || len(id) != 12 || id != strings.ToLower(id) {
		return false
	}
	_, err := strconv.ParseUint(id, 16, 48)
	return err == nil
}

func watchDockerNetworkEvents(ctx context.Context, socket string, retryDelay time.Duration, refresh func()) {
	client := localDockerClient(socket)
	for ctx.Err() == nil {
		readDockerNetworkEvents(ctx, client, refresh)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}

func readDockerNetworkEvents(ctx context.Context, client *http.Client, refresh func()) {
	now := time.Now()
	filters := url.Values{
		"filters": {`{"type":["network"],"event":["create","destroy","remove"]}`},
		// Docker flushes the headers before subscribing. Replay events from this attempt.
		"since": {fmt.Sprintf("%d.%09d", now.Unix(), now.Nanosecond())},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/events?"+filters.Encode(), nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || ctx.Err() != nil {
		return
	}

	refresh()
	decoder := json.NewDecoder(resp.Body)
	for ctx.Err() == nil {
		var event struct {
			Type   string
			Action string
			Actor  struct{ Attributes map[string]string }
		}
		if err := decoder.Decode(&event); err != nil || ctx.Err() != nil {
			return
		}
		if driver := event.Actor.Attributes["type"]; event.Type != "network" || (driver != "" && driver != "bridge") {
			continue
		}
		switch event.Action {
		case "create", "destroy", "remove":
			refresh()
		}
	}
}

func routeDockerBridges(ctx context.Context, reported map[string]struct{}) ([]dockerBridge, error) {
	if err := ctx.Err(); err != nil || len(reported) == 0 {
		return nil, err
	}
	conn, err := rtnetlink.Dial(nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	var links []*rtnetlink.LinkMessage
	for name := range reported {
		if name == "" || len(name) >= unix.IFNAMSIZ || strings.ContainsAny(name, "\x00/*\"\\\r\n\t") {
			continue
		}
		req := &rtnetlink.LinkMessage{Attributes: &rtnetlink.LinkAttributes{Name: name}}
		msgs, err := conn.Execute(req, unix.RTM_GETLINK, netlink.Request)
		if errors.Is(err, unix.ENODEV) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			link, ok := msg.(*rtnetlink.LinkMessage)
			if ok && link.Attributes != nil && link.Attributes.Info != nil && link.Attributes.Info.Kind == "bridge" {
				links = append(links, link)
			}
		}
	}

	// Strict checking enables the kernel's interface-index filter on address dumps.
	if err := conn.SetOption(netlink.GetStrictCheck, true); err != nil && !errors.Is(err, unix.ENOPROTOOPT) {
		return nil, err
	}
	var bridges []dockerBridge
	for _, link := range links {
		req := &rtnetlink.AddressMessage{Family: unix.AF_INET, Index: link.Index}
		msgs, err := conn.Execute(req, unix.RTM_GETADDR, netlink.Request|netlink.Dump)
		if errors.Is(err, unix.ENODEV) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			addr, ok := msg.(*rtnetlink.AddressMessage)
			if !ok || addr.Index != link.Index || addr.Attributes == nil {
				continue
			}
			ip, _ := netip.AddrFromSlice(addr.Attributes.Local.To4())
			if ip.IsGlobalUnicast() {
				bridges = append(bridges, dockerBridge{Name: link.Attributes.Name, Address: ip})
				break
			}
		}
	}
	slices.SortFunc(bridges, func(a, b dockerBridge) int { return strings.Compare(a.Name, b.Name) })
	if index := slices.IndexFunc(bridges, func(bridge dockerBridge) bool { return bridge.Name == "docker0" }); index > 0 {
		bridge := bridges[index]
		copy(bridges[1:index+1], bridges[:index])
		bridges[0] = bridge
	}
	return bridges, ctx.Err()
}
