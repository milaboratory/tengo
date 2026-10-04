package tengo

import "github.com/d5/tengo/v2/parser"

// maxStackDepth returns an upper bound on the number of operand stack slots a
// function body uses above its locals. Every opcode has a fixed stack effect
// (OpCall with a spread argument is bounded at run time instead), and
// compiler-generated code is structured, so the depth at each instruction is
// the same on every path reaching it and a linear pass is exact.
func maxStackDepth(insts []byte) int {
	depth, max := 0, 0
	var buf [4]int
	for ip := 0; ip < len(insts); {
		op := insts[ip]
		if int(op) >= len(parser.OpcodeOperands) {
			break // malformed; the VM reports the unknown opcode
		}
		operands, width := parser.ReadOperandsInto(buf[:0],
			parser.OpcodeOperands[op], insts[ip+1:])
		ip += 1 + width

		switch op {
		case parser.OpConstant, parser.OpNull, parser.OpTrue, parser.OpFalse,
			parser.OpGetGlobal, parser.OpGetLocal, parser.OpGetBuiltin,
			parser.OpGetFreePtr, parser.OpGetFree, parser.OpGetLocalPtr:
			depth++
		case parser.OpBinaryOp, parser.OpEqual, parser.OpNotEqual,
			parser.OpPop, parser.OpJumpFalsy, parser.OpAndJump,
			parser.OpOrJump, parser.OpSetGlobal, parser.OpIndex,
			parser.OpDefineLocal, parser.OpSetLocal, parser.OpSetFree:
			depth--
		case parser.OpSliceIndex:
			depth -= 2
		case parser.OpSetSelGlobal, parser.OpSetSelLocal, parser.OpSetSelFree:
			depth -= operands[1] + 1
		case parser.OpArray, parser.OpMap:
			depth -= operands[0] - 1
		case parser.OpCall:
			depth -= operands[0]
		case parser.OpReturn:
			depth -= operands[0]
		case parser.OpClosure:
			depth -= operands[1] - 1
		}
		if depth > max {
			max = depth
		}
	}
	return max
}
