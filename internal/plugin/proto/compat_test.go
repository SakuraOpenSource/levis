package proto

import (
	public "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"testing"
)

func TestInternalUsesCanonicalDescriptor(t *testing.T) {
	if File_plugin_proto != public.File_plugin_proto {
		t.Fatal("compatibility package must share canonical descriptor")
	}
}
