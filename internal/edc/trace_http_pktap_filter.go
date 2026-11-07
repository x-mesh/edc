package edc

import (
	"fmt"

	"golang.org/x/net/bpf"
)

const pktapHTTPPacketLimit = 256 << 10

func pktapHTTPFilter(port uint16) ([]bpf.Instruction, error) {
	return pktapTransportFilter(port, false)
}

func pktapTransportFilter(port uint16, udp bool) ([]bpf.Instruction, error) {
	var code []bpf.Instruction
	labels := map[string]int{}
	var patches []struct {
		at      int
		yes, no string
	}
	emit := func(instructions ...bpf.Instruction) { code = append(code, instructions...) }
	mark := func(label string) { labels[label] = len(code) }
	branch := func(test bpf.JumpTest, value uint32, yes, no string) {
		patches = append(patches, struct {
			at      int
			yes, no string
		}{len(code), yes, no})
		emit(bpf.JumpIf{Cond: test, Val: value})
	}
	jump := func(label string) {
		patches = append(patches, struct {
			at      int
			yes, no string
		}{len(code), label, ""})
		emit(bpf.Jump{})
	}
	loadLE := func(offset uint32, size int) {
		emit(bpf.LoadAbsolute{Off: offset + uint32(size-1), Size: 1})
		for i := size - 2; i >= 0; i-- {
			emit(bpf.ALUOpConstant{Op: bpf.ALUOpShiftLeft, Val: 8}, bpf.TAX{}, bpf.LoadAbsolute{Off: offset + uint32(i), Size: 1}, bpf.ALUOpX{Op: bpf.ALUOpOr})
		}
	}
	// PKTAP metadata is little-endian, but BPF word loads use network byte order.
	emit(bpf.LoadAbsolute{Off: 36, Size: 4})
	branch(bpf.JumpBitsSet, 0x00000800, "v2", "")
	emit(bpf.LoadAbsolute{Off: 4, Size: 4})
	branch(bpf.JumpNotEqual, 1<<24, "reject", "")
	loadLE(0, 4)
	emit(bpf.StoreScratch{Src: bpf.RegA, N: 0})
	loadLE(44, 4)
	emit(bpf.LoadScratch{Dst: bpf.RegX, N: 0}, bpf.ALUOpX{Op: bpf.ALUOpAdd}, bpf.StoreScratch{Src: bpf.RegA, N: 0}, bpf.TAX{}, bpf.LoadAbsolute{Off: 40, Size: 4})
	jump("family")
	mark("v2")
	emit(bpf.LoadAbsolute{Off: 0, Size: 1}, bpf.StoreScratch{Src: bpf.RegA, N: 0})
	loadLE(8, 2)
	emit(bpf.LoadScratch{Dst: bpf.RegX, N: 0}, bpf.ALUOpX{Op: bpf.ALUOpAdd}, bpf.StoreScratch{Src: bpf.RegA, N: 0}, bpf.TAX{}, bpf.LoadAbsolute{Off: 16, Size: 4})
	mark("family")
	branch(bpf.JumpEqual, 2<<24, "ipv4", "")
	branch(bpf.JumpEqual, 30<<24, "ipv6", "reject")
	mark("ipv4")
	emit(bpf.LoadIndirect{Off: 9, Size: 1})
	if udp {
		branch(bpf.JumpEqual, 17, "ipv4-transport", "")
	}
	branch(bpf.JumpNotEqual, 6, "reject", "")
	mark("ipv4-transport")
	emit(bpf.LoadIndirect{Off: 6, Size: 2})
	branch(bpf.JumpBitsSet, 0x3fff, "reject", "")
	if port == 0 {
		jump("accept")
	} else {
		emit(bpf.LoadIndirect{Off: 0, Size: 1}, bpf.ALUOpConstant{Op: bpf.ALUOpAnd, Val: 15}, bpf.ALUOpConstant{Op: bpf.ALUOpShiftLeft, Val: 2}, bpf.LoadScratch{Dst: bpf.RegX, N: 0}, bpf.ALUOpX{Op: bpf.ALUOpAdd}, bpf.TAX{})
		jump("ports")
	}
	mark("ipv6")
	emit(bpf.LoadIndirect{Off: 6, Size: 1})
	branch(bpf.JumpEqual, 6, "ipv6-tcp", "")
	if udp {
		branch(bpf.JumpEqual, 17, "ipv6-tcp", "")
	}
	// Preserve extension-header packets for the existing userspace TCP decoder.
	for _, protocol := range []uint32{0, 43, 51, 60} {
		branch(bpf.JumpEqual, protocol, "accept", "")
	}
	jump("reject")
	mark("ipv6-tcp")
	if port == 0 {
		jump("accept")
	} else {
		emit(bpf.LoadScratch{Dst: bpf.RegA, N: 0}, bpf.ALUOpConstant{Op: bpf.ALUOpAdd, Val: 40}, bpf.TAX{})
		mark("ports")
		emit(bpf.LoadIndirect{Off: 0, Size: 2})
		branch(bpf.JumpEqual, uint32(port), "accept", "")
		emit(bpf.LoadIndirect{Off: 2, Size: 2})
		branch(bpf.JumpEqual, uint32(port), "accept", "reject")
	}
	mark("accept")
	emit(bpf.RetConstant{Val: pktapHTTPPacketLimit})
	mark("reject")
	emit(bpf.RetConstant{Val: 0})
	for _, patch := range patches {
		resolve := func(label string) (int, error) {
			if label == "" {
				return 0, nil
			}
			target, ok := labels[label]
			if !ok || target <= patch.at {
				return 0, fmt.Errorf("invalid PKTAP filter branch %q", label)
			}
			return target - patch.at - 1, nil
		}
		yes, err := resolve(patch.yes)
		if err != nil {
			return nil, err
		}
		no, err := resolve(patch.no)
		if err != nil {
			return nil, err
		}
		switch instruction := code[patch.at].(type) {
		case bpf.JumpIf:
			if yes > 255 || no > 255 {
				return nil, fmt.Errorf("PKTAP filter branch exceeds classic BPF limits")
			}
			instruction.SkipTrue, instruction.SkipFalse = uint8(yes), uint8(no)
			code[patch.at] = instruction
		case bpf.Jump:
			instruction.Skip = uint32(yes)
			code[patch.at] = instruction
		}
	}
	return code, nil
}
