package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"yagp/internal/model"

	gormlogger "gorm.io/gorm/logger"
)

type optionQueryLog struct {
	gormlogger.Interface
	statements []string
}

func (l *optionQueryLog) Trace(_ context.Context, _ time.Time, statement func() (string, int64), _ error) {
	sql, _ := statement()
	l.statements = append(l.statements, sql)
}

func TestRoutingNodeOptionsReturnEditorFieldsInOneQuery(t *testing.T) {
	f := newHandlerHTTPFixture(t)
	f.registerNodes()
	node := model.GostNode{
		Name: "first", Address: "192.0.2.1", IPv4: "192.0.2.1", IPv6: "2001:db8::1",
		Addresses: []string{"192.0.2.1", "2001:db8::1"}, LocalAddresses: []string{"10.0.0.1"},
		ManualAddresses: []string{"198.51.100.9"}, MPTCPSupported: true,
		AddressMappings: []model.NodeAddressMapping{{ListenIP: "10.0.0.1", ConnectIP: "192.0.2.1"}},
		Remark:          "private note", TotalBytes: 350,
	}
	if err := f.db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	log := &optionQueryLog{Interface: gormlogger.Discard}
	f.db.Logger = log
	reply := f.request(t, "GET", "/nodes/options", "", 200)
	page := handlerData[struct {
		List []map[string]any
		Next uint
	}](t, reply)
	var want map[string]any
	if err := json.Unmarshal([]byte(`{"id":1,"name":"first","address":"192.0.2.1","ipv4":"192.0.2.1","ipv6":"2001:db8::1","addresses":["192.0.2.1","2001:db8::1"],"local_addresses":["10.0.0.1"],"manual_addresses":["198.51.100.9"],"address_mappings":[{"listen_ip":"10.0.0.1","connect_ip":"192.0.2.1"}],"mptcp_supported":true}`), &want); err != nil {
		t.Fatal(err)
	}
	if page.Next != 0 || !reflect.DeepEqual(page.List, []map[string]any{want}) {
		t.Fatalf("unexpected routing options: %s", reply.Data)
	}
	if len(log.statements) != 1 || strings.Contains(log.statements[0], "*") || strings.Contains(log.statements[0], "total_bytes") {
		t.Fatalf("expected a single projected node query; got %v", log.statements)
	}
}

func TestRoutingNodeOptionsContinuePastOneThousandNodes(t *testing.T) {
	f := newHandlerHTTPFixture(t)
	f.registerNodes()
	f.request(t, "GET", "/nodes/options?before=bad", "", 400)
	nodes := make([]model.GostNode, 1002)
	for i := range nodes {
		nodes[i].Name = fmt.Sprintf("node-%d", i)
	}
	if err := f.db.CreateInBatches(&nodes, 100).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Delete(&nodes[1]).Error; err != nil {
		t.Fatal(err)
	}
	type optionsPage struct {
		List []model.RoutingNodeOption
		Next uint
	}
	first := handlerData[optionsPage](t, f.request(t, "GET", "/nodes/options", "", 200))
	if len(first.List) != 1000 || first.Next != nodes[2].ID {
		t.Fatalf("first page contains %d items, next=%d", len(first.List), first.Next)
	}
	for i, option := range first.List {
		if option.ID != nodes[len(nodes)-1-i].ID {
			t.Fatalf("unexpected option %d at index %d", option.ID, i)
		}
	}
	next := handlerData[optionsPage](t, f.request(t, "GET", fmt.Sprintf("/nodes/options?before=%d", first.Next), "", 200))
	if len(next.List) != 1 || next.List[0].ID != nodes[0].ID || next.Next != 0 {
		t.Fatalf("unexpected final page: %+v", next)
	}
}
