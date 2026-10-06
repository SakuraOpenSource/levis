package proto

import (
	"google.golang.org/protobuf/reflect/protoreflect"
	"testing"
)

func TestAppendOnlyHostOperationsContract(t *testing.T) {
	f := File_plugin_proto
	values := f.Enums().ByName("HostAction").Values()
	if v := values.ByName("HOST_ACTION_RESIZE"); v == nil || v.Number() != 12 {
		t.Fatal("missing append-only RESIZE=12")
	}
	req := f.Messages().ByName("ManageHostRequest")
	for name, number := range map[protoreflect.Name]protoreflect.FieldNumber{"host_id": 1, "action": 2, "billing_cycle": 3, "os": 4, "interface_config": 5, "resources": 6, "operation_id": 7} {
		v := req.Fields().ByName(name)
		if v == nil || v.Number() != number {
			t.Fatalf("wrong field %s=%d", name, number)
		}
	}
	service := f.Services().ByName("Plugin")
	if v := service.Methods().ByName("HostOperation"); v == nil || v.Input().Name() != "HostOperationRequest" || v.Output().Name() != "HostOperationReply" {
		t.Fatal("HostOperation contract missing")
	}
	if v := service.Methods().ByName("DownloadHostBackup"); v == nil || !v.IsStreamingServer() || v.IsStreamingClient() {
		t.Fatal("backup must be server-streaming")
	}
	for message, fields := range map[protoreflect.Name][]protoreflect.Name{
		"HostOperationRequest": {"host_id", "action", "payload_json", "interface_config"},
		"HostOperationReply":   {"data_json", "error"},
		"HostBackupRequest":    {"host_id", "backup_id", "interface_config"},
		"HostBackupChunk":      {"data", "filename"},
	} {
		m := f.Messages().ByName(message)
		if m == nil {
			t.Fatalf("missing %s", message)
		}
		for i, n := range fields {
			field := m.Fields().ByName(n)
			if field == nil || field.Number() != protoreflect.FieldNumber(i+1) {
				t.Fatalf("wrong %s.%s", message, n)
			}
		}
	}
}
