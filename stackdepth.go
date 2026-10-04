package tengo

import "github.com/d5/tengo/v2/parser"

// stackEffect returns how many operand stack slots op pushes (positive) or
// pops (negative), given its decoded operands. OpCall with a spread argument
// is bounded at run time instead, since the expansion is dynamic.
func stackEffect(op byte, operands []int) int {
	switch op {
	case parser.OpConstant, parser.OpNull, parser.OpTrue, parser.OpFalse,
		parser.OpGetGlobal, parser.OpGetLocal, parser.OpGetBuiltin,
		parser.OpGetFreePtr, parser.OpGetFree, parser.OpGetLocalPtr:
		return 1
	case parser.OpBinaryOp, parser.OpEqual, parser.OpNotEqual,
		parser.OpPop, parser.OpJumpFalsy, parser.OpAndJump,
		parser.OpOrJump, parser.OpSetGlobal, parser.OpIndex,
		parser.OpDefineLocal, parser.OpSetLocal, parser.OpSetFree:
		return -1
	case parser.OpSliceIndex:
		return -2
	case parser.OpSetSelGlobal, parser.OpSetSelLocal, parser.OpSetSelFree:
		return -(operands[1] + 1)
	case parser.OpArray, parser.OpMap:
		return -(operands[0] - 1)
	case parser.OpCall:
		return -operands[0]
	case parser.OpReturn:
		return -operands[0]
	case parser.OpClosure:
		return -(operands[1] - 1)
	}
	return 0
}

// scanDepth walks insts in order and calls visit for every instruction with
// its decoded operands and the operand stack depth before it. Every opcode
// has a fixed stack effect and compiler-generated code is structured, so the
// depth at an instruction is the same on every path reaching it. The depth
// is carried along the fall-through path, except after an unconditional
// jump or a return, where the next instruction is only reachable by a jump
// and takes the depth recorded by that jump (a ternary's else branch, for
// instance, starts below where the then branch ended).
func scanDepth(insts []byte, visit func(ip int, op byte, operands []int, depth int)) {
	depthAt := make([]int, len(insts)+1)
	for i := range depthAt {
		depthAt[i] = -1
	}
	record := func(target, depth int) {
		if target >= 0 && target <= len(insts) && depthAt[target] < 0 {
			depthAt[target] = depth
		}
	}
	var buf [4]int
	depth := 0
	noFallthrough := false
	for ip := 0; ip < len(insts); {
		op := insts[ip]
		if int(op) >= len(parser.OpcodeOperands) {
			visit(ip, op, nil, depth)
			break // malformed; the VM reports the unknown opcode
		}
		if noFallthrough && depthAt[ip] >= 0 {
			depth = depthAt[ip]
		}
		operands, width := parser.ReadOperandsInto(buf[:0],
			parser.OpcodeOperands[op], insts[ip+1:])
		visit(ip, op, operands, depth)
		switch op {
		case parser.OpJump:
			record(operands[0], depth)
		case parser.OpJumpFalsy:
			record(operands[0], depth-1)
		case parser.OpAndJump, parser.OpOrJump:
			record(operands[0], depth) // the value stays on the jump path
		}
		noFallthrough = op == parser.OpJump || op == parser.OpReturn
		depth += stackEffect(op, operands)
		ip += 1 + width
	}
}

// maxStackDepth returns the number of operand stack slots a function body
// uses above its locals.
func maxStackDepth(insts []byte) int {
	max := 0
	scanDepth(insts, func(ip int, op byte, operands []int, depth int) {
		if operands == nil {
			return
		}
		if after := depth + stackEffect(op, operands); after > max {
			max = after
		}
	})
	return max
}
