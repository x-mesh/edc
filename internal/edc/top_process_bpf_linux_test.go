//go:build linux

package edc

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cilium/ebpf/btf"
)

func topBTFSpec(t *testing.T, types ...btf.Type) *btf.Spec {
	t.Helper()
	builder, err := btf.NewBuilder(types, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := builder.Marshal(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := btf.LoadSpecFromReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func topTracepointType(name string, params int) btf.Type {
	void := &btf.Void{}
	proto := &btf.FuncProto{Return: void}
	for range params {
		proto.Params = append(proto.Params, btf.FuncParam{Type: &btf.Int{Name: "long", Size: 8}})
	}
	return &btf.Typedef{Name: "btf_trace_" + name, Type: &btf.Pointer{Target: proto}}
}

func TestTracepointParamsCountsTheFunctionPrototype(t *testing.T) {
	spec := topBTFSpec(t, topTracepointType("sched_switch", 5), topTracepointType("block_rq_complete", 3))
	if got, err := tracepointParams(spec, "sched_switch"); err != nil || got != 5 {
		t.Fatalf("sched_switch = %d, %v", got, err)
	}
	if err := topProcessKernelSupported(spec); err == nil || !strings.Contains(err.Error(), "block_rq_complete has 2 arguments") && !strings.Contains(err.Error(), "no tracepoint") {
		t.Fatalf("a kernel with a short tracepoint or missing tracepoints must be unsupported: %v", err)
	}
	if _, err := tracepointParams(spec, "missing"); err == nil {
		t.Fatal("a missing tracepoint must be an error")
	}
}

// BTF에 같은 이름의 struct가 둘 이상인 kernel(OrbStack 7.0)에서 하나라도 member를 가지면 통과한다.
func TestBTFStructHasMemberAcceptsDuplicateStructs(t *testing.T) {
	u32 := &btf.Int{Name: "u32", Size: 4}
	task := func(fields ...string) *btf.Struct {
		members := make([]btf.Member, len(fields))
		for index, field := range fields {
			members[index] = btf.Member{Name: field, Type: u32, Offset: btf.Bits(32 * index)}
		}
		return &btf.Struct{Name: "task_struct", Size: uint32(4 * len(fields)), Members: members}
	}
	if err := btfStructHasMember(topBTFSpec(t, task("pid"), task("pid", "signal")), "task_struct", "signal"); err != nil {
		t.Fatalf("duplicate task_struct: %v", err)
	}
	if err := btfStructHasMember(topBTFSpec(t, task("pid")), "task_struct", "signal"); err == nil || !strings.Contains(err.Error(), "task_struct.signal") {
		t.Fatalf("missing field: %v", err)
	}
	if err := btfStructHasMember(topBTFSpec(t, u32), "task_struct", "signal"); err == nil || !strings.Contains(err.Error(), "no type task_struct") {
		t.Fatalf("missing type: %v", err)
	}
}
