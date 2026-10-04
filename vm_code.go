package tengo

import (
	"github.com/d5/tengo/v2/parser"
	"github.com/d5/tengo/v2/token"
)

// The VM does not interpret CompiledFunction.Instructions directly. Each
// function is translated once into a slice of pre-decoded instrs in which
// operand stack slots have become frame-relative registers: the operand
// stack depth before each instruction is static (compiler output is
// structured and every opcode has a fixed stack effect), so the slot an
// instruction reads or writes is known at translation time and no stack
// pointer is maintained while running. Operands carry a kind, which lets a
// peephole pass fold loads into their consumers and stores into producers,
// so `s += i * 3` runs as one instruction instead of five.
//
// The serialized bytecode format and the compiler are unchanged; the
// translation is an execution detail and is rebuilt whenever Instructions
// change (see Bytecode.RemoveDuplicates).

// instr is a pre-decoded instruction. The meaning of a, b and c depends on
// op; pos is the offset of the originating opcode in Instructions, used for
// source positions in error messages.
type instr struct {
	op  uint8
	tok token.Token // operator for opBinary and opBinaryJF
	a   int32
	b   int32
	c   int32
	pos int32
}

// Operand kinds, stored in the top byte of an operand. The low 24 bits hold
// the index.
const (
	kTemp      = iota // operand stack slot, frame-relative (never *ObjectPtr)
	kLocal            // local variable slot, frame-relative; read derefs *ObjectPtr, write stores plainly
	kLocalSet         // local as destination with OpSetLocal semantics: write through *ObjectPtr
	kConst            // constants[idx]
	kGlobal           // globals[idx]
	kFree             // *freeVars[idx].Value
	kFreePtr          // freeVars[idx] itself (source only)
	kBuiltin          // builtinFuncs[idx] (source only)
	kUndefined        // literal (source only)
	kTrue
	kFalse
)

const operandMask = 1<<24 - 1

func operand(kind int, idx int) int32 {
	if idx < 0 {
		idx = 0 // malformed input; keep the encoding valid
	}
	return int32(kind<<24 | (idx & operandMask))
}

func isTemp(o int32) bool { return o>>24 == kTemp }

// Translated opcodes.
const (
	opMove          uint8 = iota // a=dst, b=src
	opBinary                     // a=dst, b=lhs, c=rhs, tok
	opBinaryJF                   // b=lhs, c=rhs, tok; a=target: jump if comparison is false
	opEqual                      // a=dst, b=lhs, c=rhs
	opNotEqual                   // a=dst, b=lhs, c=rhs
	opEqualJF                    // b=lhs, c=rhs; a=target: jump if not equal
	opNotEqualJF                 // b=lhs, c=rhs; a=target: jump if equal
	opLNot                       // a=dst, b=src
	opBComplement                // a=dst, b=src
	opMinus                      // a=dst, b=src
	opJumpFalsy                  // b=src; a=target
	opAndJump                    // b=temp slot (value kept on jump); a=target
	opOrJump                     // b=temp slot; a=target
	opJump                       // a=target
	opSetSelGlobal               // a=global, b=numSelectors, c=slot of value (selectors follow)
	opSetSelLocal                // a=local, b=numSelectors, c=slot of value
	opSetSelFree                 // a=free, b=numSelectors, c=slot of value
	opArray                      // a=dst slot (first element), b=numElements
	opMap                        // a=dst slot (first key), b=numElements
	opError                      // a=slot, in place
	opImmutable                  // a=slot, in place
	opIndex                      // a=dst, b=container, c=index
	opSliceIndex                 // a=slot of container (low, high follow), result in place
	opCall                       // a=slot of callee (args follow), b=numArgs, c=spread
	opReturn                     // b=src
	opReturn0                    // return undefined
	opGetLocalPtr                // a=dst slot, b=local index
	opClosure                    // a=dst slot (free vars start there), b=const index, c=numFree
	opIteratorInit               // a=slot, in place
	opIteratorNext               // a=slot, in place
	opIteratorKey                // a=slot, in place
	opIteratorValue              // a=slot, in place
	opSuspend                    // a=frame-relative slot count in use (for IsStackEmpty)
	opInvalid                    // a=original opcode

	// specialized moves, chosen by specialize for the common operand kinds
	// so an unfused move costs no more than the stack op it replaces
	opLoadConst     // a=temp slot, b=const index
	opLoadGlobal    // a=temp slot, b=global index
	opLoadLocal     // a=temp slot, b=local index
	opLoadFree      // a=temp slot, b=free index
	opStoreGlobal   // a=global index, b=temp slot
	opStoreLocal    // a=local index, b=temp slot (OpDefineLocal)
	opStoreLocalSet // a=local index, b=temp slot (OpSetLocal)
	opStoreFree     // a=free index, b=temp slot
)

// specialize rewrites generic moves between a temp and one of the common
// operand kinds into dedicated opcodes with plain indexes.
func specialize(in *instr) {
	if in.op != opMove {
		return
	}
	ka, ia := int(in.a>>24), in.a&operandMask
	kb, ib := int(in.b>>24), in.b&operandMask
	switch {
	case ka == kTemp && kb == kConst:
		in.op, in.a, in.b = opLoadConst, ia, ib
	case ka == kTemp && kb == kGlobal:
		in.op, in.a, in.b = opLoadGlobal, ia, ib
	case ka == kTemp && kb == kLocal:
		in.op, in.a, in.b = opLoadLocal, ia, ib
	case ka == kTemp && kb == kFree:
		in.op, in.a, in.b = opLoadFree, ia, ib
	case kb == kTemp && ka == kGlobal:
		in.op, in.a, in.b = opStoreGlobal, ia, ib
	case kb == kTemp && ka == kLocal:
		in.op, in.a, in.b = opStoreLocal, ia, ib
	case kb == kTemp && ka == kLocalSet:
		in.op, in.a, in.b = opStoreLocalSet, ia, ib
	case kb == kTemp && ka == kFree:
		in.op, in.a, in.b = opStoreFree, ia, ib
	}
}

// hasJump reports whether operand a of op is a jump target.
func hasJump(op uint8) bool {
	switch op {
	case opBinaryJF, opEqualJF, opNotEqualJF, opJumpFalsy, opAndJump,
		opOrJump, opJump:
		return true
	}
	return false
}

// codeFor returns the translated code of fn, translating on first use.
//
// Script.Compile and Bytecode.Decode translate every function up front.
// The lazy path here only serves bytecode assembled by hand; such a function
// must not be shared between VMs running concurrently before its first use.
func codeFor(fn *CompiledFunction) []instr {
	if fn.code == nil {
		fn.code = translate(fn.Instructions, fn.NumLocals)
	}
	return fn.code
}

// translate converts stack bytecode into register-form instrs.
func translate(insts []byte, numLocals int) []instr {
	var buf [4]int

	// pass 1: jump targets, as offsets into insts
	isTarget := make([]bool, len(insts)+1)
	for ip := 0; ip < len(insts); {
		op := insts[ip]
		if int(op) >= len(parser.OpcodeOperands) {
			break
		}
		operands, width := parser.ReadOperandsInto(buf[:0],
			parser.OpcodeOperands[op], insts[ip+1:])
		switch op {
		case parser.OpJump, parser.OpJumpFalsy, parser.OpAndJump,
			parser.OpOrJump:
			if t := operands[0]; t >= 0 && t <= len(insts) {
				isTarget[t] = true
			}
		}
		ip += 1 + width
	}

	// pass 2: one instr per opcode with static slots. byteIdx maps an
	// offset in insts to the index of the instr emitted for it (or, for
	// OpPop, which emits nothing, to the next one).
	code := make([]instr, 0, len(insts)/2+1)
	byteIdx := make([]int32, len(insts)+1)
	t := func(d int) int32 { return operand(kTemp, numLocals+d) }
	scanDepth(insts, func(ip int, op byte, operands []int, depth int) {
		byteIdx[ip] = int32(len(code))
		if operands == nil {
			code = append(code, instr{op: opInvalid, a: int32(op), pos: int32(ip)})
			return
		}
		in := instr{pos: int32(ip)}
		switch op {
		case parser.OpConstant:
			in.op, in.a, in.b = opMove, t(depth), operand(kConst, operands[0])
		case parser.OpNull:
			in.op, in.a, in.b = opMove, t(depth), operand(kUndefined, 0)
		case parser.OpTrue:
			in.op, in.a, in.b = opMove, t(depth), operand(kTrue, 0)
		case parser.OpFalse:
			in.op, in.a, in.b = opMove, t(depth), operand(kFalse, 0)
		case parser.OpGetGlobal:
			in.op, in.a, in.b = opMove, t(depth), operand(kGlobal, operands[0])
		case parser.OpSetGlobal:
			in.op, in.a, in.b = opMove, operand(kGlobal, operands[0]), t(depth-1)
		case parser.OpGetLocal:
			in.op, in.a, in.b = opMove, t(depth), operand(kLocal, operands[0])
		case parser.OpDefineLocal:
			in.op, in.a, in.b = opMove, operand(kLocal, operands[0]), t(depth-1)
		case parser.OpSetLocal:
			in.op, in.a, in.b = opMove, operand(kLocalSet, operands[0]), t(depth-1)
		case parser.OpGetFree:
			in.op, in.a, in.b = opMove, t(depth), operand(kFree, operands[0])
		case parser.OpSetFree:
			in.op, in.a, in.b = opMove, operand(kFree, operands[0]), t(depth-1)
		case parser.OpGetFreePtr:
			in.op, in.a, in.b = opMove, t(depth), operand(kFreePtr, operands[0])
		case parser.OpGetBuiltin:
			in.op, in.a, in.b = opMove, t(depth), operand(kBuiltin, operands[0])
		case parser.OpGetLocalPtr:
			in.op, in.a, in.b = opGetLocalPtr, t(depth), int32(operands[0])
		case parser.OpBinaryOp:
			in.op, in.tok = opBinary, token.Token(operands[0])
			in.a, in.b, in.c = t(depth-2), t(depth-2), t(depth-1)
		case parser.OpEqual:
			in.op, in.a, in.b, in.c = opEqual, t(depth-2), t(depth-2), t(depth-1)
		case parser.OpNotEqual:
			in.op, in.a, in.b, in.c = opNotEqual, t(depth-2), t(depth-2), t(depth-1)
		case parser.OpPop:
			return
		case parser.OpLNot:
			in.op, in.a, in.b = opLNot, t(depth-1), t(depth-1)
		case parser.OpBComplement:
			in.op, in.a, in.b = opBComplement, t(depth-1), t(depth-1)
		case parser.OpMinus:
			in.op, in.a, in.b = opMinus, t(depth-1), t(depth-1)
		case parser.OpJumpFalsy:
			in.op, in.a, in.b = opJumpFalsy, int32(operands[0]), t(depth-1)
		case parser.OpAndJump:
			in.op, in.a, in.b = opAndJump, int32(operands[0]), t(depth-1)
		case parser.OpOrJump:
			in.op, in.a, in.b = opOrJump, int32(operands[0]), t(depth-1)
		case parser.OpJump:
			in.op, in.a = opJump, int32(operands[0])
		case parser.OpSetSelGlobal:
			in.op, in.a, in.b = opSetSelGlobal, int32(operands[0]), int32(operands[1])
			in.c = t(depth - operands[1] - 1)
		case parser.OpSetSelLocal:
			in.op, in.a, in.b = opSetSelLocal, int32(operands[0]), int32(operands[1])
			in.c = t(depth - operands[1] - 1)
		case parser.OpSetSelFree:
			in.op, in.a, in.b = opSetSelFree, int32(operands[0]), int32(operands[1])
			in.c = t(depth - operands[1] - 1)
		case parser.OpArray:
			in.op, in.a, in.b = opArray, t(depth-operands[0]), int32(operands[0])
		case parser.OpMap:
			in.op, in.a, in.b = opMap, t(depth-operands[0]), int32(operands[0])
		case parser.OpError:
			in.op, in.a = opError, t(depth-1)
		case parser.OpImmutable:
			in.op, in.a = opImmutable, t(depth-1)
		case parser.OpIndex:
			in.op, in.a, in.b, in.c = opIndex, t(depth-2), t(depth-2), t(depth-1)
		case parser.OpSliceIndex:
			in.op, in.a = opSliceIndex, t(depth-3)
		case parser.OpCall:
			in.op, in.a = opCall, t(depth-1-operands[0])
			in.b, in.c = int32(operands[0]), int32(operands[1])
		case parser.OpReturn:
			if operands[0] == 1 {
				in.op, in.b = opReturn, t(depth-1)
			} else {
				in.op = opReturn0
			}
		case parser.OpClosure:
			in.op, in.a = opClosure, t(depth-operands[1])
			in.b, in.c = int32(operands[0]), int32(operands[1])
		case parser.OpIteratorInit:
			in.op, in.a = opIteratorInit, t(depth-1)
		case parser.OpIteratorNext:
			in.op, in.a = opIteratorNext, t(depth-1)
		case parser.OpIteratorKey:
			in.op, in.a = opIteratorKey, t(depth-1)
		case parser.OpIteratorValue:
			in.op, in.a = opIteratorValue, t(depth-1)
		case parser.OpSuspend:
			in.op, in.a = opSuspend, int32(numLocals+depth)
		default:
			in.op, in.a = opInvalid, int32(op)
		}
		code = append(code, in)
	})
	byteIdx[len(insts)] = int32(len(code))

	// jump targets per emitted instr
	target := make([]bool, len(code)+1)
	for off, is := range isTarget {
		if is {
			target[byteIdx[off]] = true
		}
	}

	// pass 3: peephole fusion. Each output instr covers a contiguous range
	// of pass-2 instrs starting at start[k]; a jump into that range lands on
	// the output instr, so the range may contain a jump target only at an
	// instr whose predecessors in the range are all loads it needs anyway.
	out := make([]instr, 0, len(code))
	start := make([]int32, 0, len(code))
	for i := range code {
		in := code[i]
		first := int32(i)
		if !target[i] && len(out) > 0 {
			if fuse(&out[len(out)-1], &in) {
				continue
			}
			// fold the loads feeding in, most recent first. Once a load
			// that is a jump target has been folded, stop: a jump to it
			// must not re-execute an earlier load.
			absorbedTarget := false
			for len(out) > 0 && !absorbedTarget {
				last := len(out) - 1
				if !drop(&out[last], &in) {
					break
				}
				first = start[last]
				absorbedTarget = target[first]
				out, start = out[:last], start[:last]
			}
		}
		out = append(out, in)
		start = append(start, first)
	}

	// remap maps a pass-2 index to the output instr covering it
	remap := make([]int32, len(code)+1)
	for k := range out {
		end := len(code)
		if k+1 < len(out) {
			end = int(start[k+1])
		}
		for j := int(start[k]); j < end; j++ {
			remap[j] = int32(k)
		}
	}
	remap[len(code)] = int32(len(out))

	// resolve jump targets: byte offset -> pass-2 index -> final index
	for i := range out {
		if hasJump(out[i].op) {
			off := int(out[i].a)
			if off < 0 || off > len(insts) {
				off = len(insts)
			}
			out[i].a = remap[byteIdx[off]]
		}
		specialize(&out[i])
	}
	return out
}

// isCompare reports whether tok is a comparison operator whose result feeds
// a conditional jump without being materialized.
func isCompare(tok token.Token) bool {
	switch tok {
	case token.Less, token.Greater, token.LessEq, token.GreaterEq:
		return true
	}
	return false
}

// fuse tries to absorb in (which immediately follows last and is not a jump
// target) into last, rewriting last in place. It returns true on success.
func fuse(last, in *instr) bool {
	switch in.op {
	case opMove:
		// a store of the temp that last produced: retarget last
		if isTemp(in.b) && !isTemp(in.a) {
			switch last.op {
			case opMove, opBinary, opEqual, opNotEqual, opLNot,
				opBComplement, opMinus, opIndex:
				if last.a == in.b {
					last.a = in.a
					return true
				}
			}
		}
	case opJumpFalsy:
		if isTemp(in.b) && last.a == in.b {
			switch last.op {
			case opMove:
				// jump on a loaded value directly
				*last = instr{op: opJumpFalsy, a: in.a, b: last.b, pos: in.pos}
				return true
			case opBinary:
				if isCompare(last.tok) {
					*last = instr{op: opBinaryJF, tok: last.tok, a: in.a,
						b: last.b, c: last.c, pos: last.pos}
					return true
				}
			case opEqual:
				*last = instr{op: opEqualJF, a: in.a, b: last.b, c: last.c,
					pos: last.pos}
				return true
			case opNotEqual:
				*last = instr{op: opNotEqualJF, a: in.a, b: last.b,
					c: last.c, pos: last.pos}
				return true
			}
		}
	}
	return false
}

// drop tries to fold last, a load into a temp, into in, which consumes that
// temp as a source. On success in is rewritten and last is to be discarded.
func drop(last, in *instr) bool {
	if last.op != opMove || !isTemp(last.a) {
		return false
	}
	switch in.op {
	case opBinary, opEqual, opNotEqual, opIndex:
		// the right operand is loaded last; the left one right before it
		if in.c == last.a {
			in.c = last.b
			return true
		}
		if in.b == last.a && !isTemp(in.c) {
			in.b = last.b
			return true
		}
	case opLNot, opBComplement, opMinus, opReturn:
		if in.b == last.a {
			in.b = last.b
			return true
		}
	}
	return false
}
