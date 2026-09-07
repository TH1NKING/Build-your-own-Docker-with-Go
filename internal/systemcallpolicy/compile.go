package systemcallpolicy

import (
	"errors"
	"fmt"
)

const (
	bpfLoadWordAbsolute = 0x20
	bpfAndConstant      = 0x54
	bpfJumpAlways       = 0x05
	bpfJumpEqual        = 0x15
	bpfJumpGreater      = 0x25
	bpfJumpGreaterEqual = 0x35
	bpfJumpBitsSet      = 0x45
	bpfReturnConstant   = 0x06
	seccompReturnAllow  = 0x7fff0000
	seccompReturnErrno  = 0x00050000
	auditArchAMD64      = 0xc000003e
	x32SystemCallBit    = 0x40000000
	maximumInstructions = 4096
)

// This layout-independent representation lets every supported build host
// validate the same native-amd64 filter before producing a Profile Bundle.
type instruction struct {
	code   uint16
	jt, jf uint8
	k      uint32
}

type jumpTarget struct {
	index int
	label int
}

type assembler struct {
	program []instruction
	labels  map[int]int
	jumps   []jumpTarget
	next    int
	err     error
}

func compileInstructions(policy Policy) ([]instruction, error) {
	build := assembler{labels: make(map[int]int)}
	deny := build.newLabel()
	denial := uint32(seccompReturnErrno | policy.DefaultAction.Errno)
	// seccomp_data.arch is at byte 4 and nr at byte 0. x32 shares the
	// amd64 audit architecture; it must be rejected separately by its nr bit.
	build.emit(instruction{code: bpfLoadWordAbsolute, k: 4})
	build.emit(instruction{code: bpfJumpEqual, k: auditArchAMD64, jt: 1})
	build.emit(instruction{code: bpfReturnConstant, k: denial})
	build.emit(instruction{code: bpfLoadWordAbsolute, k: 0})
	build.emit(instruction{code: bpfJumpBitsSet, k: x32SystemCallBit, jf: 1})
	build.emit(instruction{code: bpfReturnConstant, k: denial})
	for _, rule := range policy.Rules {
		arguments, nextRule := build.newLabel(), build.newLabel()
		build.emit(instruction{code: bpfLoadWordAbsolute, k: 0})
		for index, name := range rule.Names {
			nextName := nextRule
			if index != len(rule.Names)-1 {
				nextName = build.newLabel()
			}
			build.condition(bpfJumpEqual, amd64SystemCalls[name], arguments, nextName)
			if nextName != nextRule {
				build.mark(nextName)
			}
		}
		build.mark(arguments)
		for _, argument := range rule.Arguments {
			accepted := build.newLabel()
			build.argument(argument, accepted, deny)
			build.mark(accepted)
		}
		build.emit(instruction{code: bpfReturnConstant, k: seccompReturnAllow})
		build.mark(nextRule)
	}
	build.mark(deny)
	build.emit(instruction{code: bpfReturnConstant, k: denial})
	if build.err != nil {
		return nil, build.err
	}
	for _, jump := range build.jumps {
		target, found := build.labels[jump.label]
		if !found || target <= jump.index || target >= len(build.program) {
			return nil, errors.New("System Call Policy has an invalid forward branch")
		}
		build.program[jump.index].k = uint32(target - jump.index - 1)
	}
	return build.program, nil
}

func (build *assembler) newLabel() int {
	build.next++
	return build.next
}

func (build *assembler) mark(label int) { build.labels[label] = len(build.program) }

func (build *assembler) emit(value instruction) {
	if len(build.program) >= maximumInstructions {
		build.err = fmt.Errorf("System Call Policy exceeds the %d-instruction kernel filter limit", maximumInstructions)
		return
	}
	build.program = append(build.program, value)
}

func (build *assembler) jump(label int) {
	build.jumps = append(build.jumps, jumpTarget{index: len(build.program), label: label})
	build.emit(instruction{code: bpfJumpAlways})
}

// Conditional offsets are only eight bits. Two local JA instructions carry
// full-width forward offsets, so a large rule cannot wrap a branch distance.
func (build *assembler) condition(operation uint16, value uint32, yes, no int) {
	build.emit(instruction{code: operation, k: value, jf: 1})
	build.jump(yes)
	build.jump(no)
}

func (build *assembler) argument(argument Argument, accepted, denied int) {
	// amd64 is little-endian. Compare the high word first, using unsigned
	// lexicographic order, then the low word only when the high words agree.
	lowOffset := uint32(16 + argument.Index*8)
	high, low := uint32(argument.Value>>32), uint32(argument.Value)
	checkLow := build.newLabel()
	build.emit(instruction{code: bpfLoadWordAbsolute, k: lowOffset + 4})
	switch argument.Operator {
	case "equal", "masked-equal":
		if argument.Operator == "masked-equal" {
			build.emit(instruction{code: bpfAndConstant, k: uint32(argument.Mask >> 32)})
		}
		build.condition(bpfJumpEqual, high, checkLow, denied)
	case "not-equal":
		build.condition(bpfJumpEqual, high, checkLow, accepted)
	case "less-than", "less-or-equal":
		checkEqual := build.newLabel()
		build.condition(bpfJumpGreater, high, denied, checkEqual)
		build.mark(checkEqual)
		build.condition(bpfJumpEqual, high, checkLow, accepted)
	case "greater-than", "greater-or-equal":
		checkEqual := build.newLabel()
		build.condition(bpfJumpGreater, high, accepted, checkEqual)
		build.mark(checkEqual)
		build.condition(bpfJumpEqual, high, checkLow, denied)
	}
	build.mark(checkLow)
	build.emit(instruction{code: bpfLoadWordAbsolute, k: lowOffset})
	switch argument.Operator {
	case "equal", "masked-equal":
		if argument.Operator == "masked-equal" {
			build.emit(instruction{code: bpfAndConstant, k: uint32(argument.Mask)})
		}
		build.condition(bpfJumpEqual, low, accepted, denied)
	case "not-equal":
		build.condition(bpfJumpEqual, low, denied, accepted)
	case "less-than":
		build.condition(bpfJumpGreaterEqual, low, denied, accepted)
	case "less-or-equal":
		build.condition(bpfJumpGreater, low, denied, accepted)
	case "greater-than":
		build.condition(bpfJumpGreater, low, accepted, denied)
	case "greater-or-equal":
		build.condition(bpfJumpGreaterEqual, low, accepted, denied)
	}
}
