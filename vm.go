package tengo

import (
	"fmt"
	"sync/atomic"

	"github.com/d5/tengo/v2/parser"
	"github.com/d5/tengo/v2/token"
)

// frame represents a function call frame.
type frame struct {
	fn          *CompiledFunction
	freeVars    []*ObjectPtr
	ip          int // index into fn's translated code of the call instruction
	basePointer int
	// discard is set when a tail call reused this frame for a call whose
	// result the function body dropped (`f(x); return`). The function then
	// returns undefined, whatever the final callee returns.
	discard bool
}

// VM is a virtual machine that executes the bytecode compiled by Compiler.
type VM struct {
	constants   []Object
	stack       [StackSize]Object
	sp          int
	globals     []Object
	fileSet     *parser.SourceFileSet
	frames      [MaxFrames]frame
	framesIndex int
	framesHigh  int // highest framesIndex reached; frames below it hold references
	curFrame    *frame
	ip          int
	aborting    int64
	maxAllocs   int64
	allocs      int64
	err         error
	modules     map[*CompiledFunction]Object // memoized module exports for this run
	ints        []Int                        // bump-allocated Int slab, see newInt
	floats      []Float                      // bump-allocated Float slab, see newFloat
}

// numSlabSize is the number of Int or Float objects carved out of one heap
// allocation by the arithmetic fast paths. Boxing every intermediate number
// is the dominant cost of numeric scripts, and a bump allocator is an order
// of magnitude cheaper than mallocgc per object. The trade-off is retention:
// a slab stays alive while any number in it is referenced, so a script that
// keeps one number out of every numSlabSize it computes retains the whole
// slab. Int and Float are 8 bytes, so a slab is 256 bytes.
const numSlabSize = 32

// newInt returns an Int holding x, from the shared small-int table or the
// VM's slab. Ints are never mutated after creation, so sharing is safe.
func (v *VM) newInt(x int64) *Int {
	if x >= smallIntMin && x <= smallIntMax {
		return &smallInts[x-smallIntMin]
	}
	if len(v.ints) == 0 {
		v.ints = make([]Int, numSlabSize)
	}
	p := &v.ints[0]
	p.Value = x
	v.ints = v.ints[1:]
	return p
}

// newFloat returns a Float holding x from the VM's slab.
func (v *VM) newFloat(x float64) *Float {
	if len(v.floats) == 0 {
		v.floats = make([]Float, numSlabSize)
	}
	p := &v.floats[0]
	p.Value = x
	v.floats = v.floats[1:]
	return p
}

// NewVM creates a VM.
func NewVM(
	bytecode *Bytecode,
	globals []Object,
	maxAllocs int64,
) *VM {
	v := new(VM)
	v.init(bytecode, globals, maxAllocs)
	return v
}

// init prepares v (new or recycled) to execute bytecode.
func (v *VM) init(bytecode *Bytecode, globals []Object, maxAllocs int64) {
	if globals == nil {
		globals = make([]Object, GlobalsSize)
	}
	bytecode.prepare()
	v.constants = bytecode.Constants
	v.sp = 0
	v.globals = globals
	v.fileSet = bytecode.FileSet
	v.framesIndex = 1
	v.framesHigh = 1
	v.ip = -1
	v.maxAllocs = maxAllocs
	v.frames[0].fn = bytecode.MainFunction
	v.frames[0].ip = -1
	v.curFrame = &v.frames[0]
}

// release drops every reference the VM holds to script objects so a pooled
// VM does not keep the last run's garbage alive.
func (v *VM) release() {
	v.stack = [StackSize]Object{}
	high := v.framesHigh
	if high > MaxFrames {
		high = MaxFrames
	}
	for i := 0; i < high; i++ {
		v.frames[i] = frame{}
	}
	v.constants = nil
	v.globals = nil
	v.fileSet = nil
	v.curFrame = nil
	v.modules = nil
	v.err = nil
}

// Abort aborts the execution.
func (v *VM) Abort() {
	atomic.StoreInt64(&v.aborting, 1)
}

// Run starts the execution.
func (v *VM) Run() (err error) {
	// reset VM states
	v.sp = 0
	v.curFrame = &(v.frames[0])
	v.curFrame.basePointer = 0
	v.framesIndex = 1
	v.ip = -1
	v.allocs = v.maxAllocs + 1
	v.modules = nil
	v.err = nil

	if v.curFrame.fn.stackDepth > StackSize {
		v.err = ErrStackOverflow
	} else {
		v.run()
	}
	atomic.StoreInt64(&v.aborting, 0)
	err = v.err
	if err != nil {
		filePos := v.fileSet.Position(
			v.curFrame.fn.SourcePos(codePos(v.curFrame.fn, v.ip)))
		err = fmt.Errorf("Runtime Error: %w\n\tat %s",
			err, filePos)
		for v.framesIndex > 1 {
			v.framesIndex--
			v.curFrame = &v.frames[v.framesIndex-1]
			filePos = v.fileSet.Position(
				v.curFrame.fn.SourcePos(codePos(v.curFrame.fn, v.curFrame.ip)))
			err = fmt.Errorf("%w\n\tat %s", err, filePos)
		}
		return err
	}
	return nil
}

// codePos returns the Instructions offset of the instruction at index ip of
// fn's translated code, or -1.
func codePos(fn *CompiledFunction, ip int) int {
	code := codeFor(fn)
	if ip < 0 || ip >= len(code) {
		return -1
	}
	return int(code[ip].pos)
}

// load reads a source operand (see the operand kinds in vm_code.go). The
// common kinds are handled inline.
func (v *VM) load(o int32, bp int, fr *frame) Object {
	idx := int(o & operandMask)
	switch o >> 24 {
	case kTemp:
		return v.stack[bp+idx]
	case kConst:
		return v.constants[idx]
	case kGlobal:
		return v.globals[idx]
	}
	return v.loadSlow(o, bp, fr)
}

func (v *VM) loadSlow(o int32, bp int, fr *frame) Object {
	idx := int(o & operandMask)
	switch o >> 24 {
	case kLocal:
		val := v.stack[bp+idx]
		if p, ok := val.(*ObjectPtr); ok {
			return *p.Value
		}
		return val
	case kFree:
		return *fr.freeVars[idx].Value
	case kFreePtr:
		return fr.freeVars[idx]
	case kBuiltin:
		return builtinFuncs[idx]
	case kTrue:
		return TrueValue
	case kFalse:
		return FalseValue
	}
	return UndefinedValue
}

// store writes a destination operand.
func (v *VM) store(o int32, bp int, fr *frame, val Object) {
	idx := int(o & operandMask)
	switch o >> 24 {
	case kTemp, kLocal:
		v.stack[bp+idx] = val
	case kGlobal:
		v.globals[idx] = val
	default:
		v.storeSlow(o, bp, fr, val)
	}
}

func (v *VM) storeSlow(o int32, bp int, fr *frame, val Object) {
	idx := int(o & operandMask)
	switch o >> 24 {
	case kLocalSet:
		// update the pointee instead of replacing the pointer: free
		// variables may reference this local
		lp := bp + idx
		if p, ok := v.stack[lp].(*ObjectPtr); ok {
			*p.Value = val
			val = p
		}
		v.stack[lp] = val
	case kFree:
		*fr.freeVars[idx].Value = val
	}
}

// run is the interpreter loop over the translated code of the current
// function. The hot state lives in locals and is written back to v on exit
// so error reporting sees it.
func (v *VM) run() {
	var (
		ip          = v.ip
		curFrame    = v.curFrame
		code        = codeFor(curFrame.fn)
		bp          = curFrame.basePointer
		stack       = &v.stack
		constants   = v.constants
		globals     = v.globals
		framesIndex = v.framesIndex
		allocs      = v.allocs
	)

	// The abort flag is checked only on jumps and calls: any non-terminating
	// execution must repeatedly take a backward jump or perform a call, so this
	// is enough to stop it, and it keeps the atomic load off the straight-line
	// dispatch path.
	for {
		ip++
		in := &code[ip]

		switch in.op {
		case opLoadConst:
			stack[bp+int(in.a)] = constants[in.b]
		case opLoadGlobal:
			stack[bp+int(in.a)] = globals[in.b]
		case opLoadLocal:
			val := stack[bp+int(in.b)]
			if p, ok := val.(*ObjectPtr); ok {
				val = *p.Value
			}
			stack[bp+int(in.a)] = val
		case opLoadFree:
			stack[bp+int(in.a)] = *curFrame.freeVars[in.b].Value
		case opStoreGlobal:
			globals[in.a] = stack[bp+int(in.b)]
		case opStoreLocal:
			stack[bp+int(in.a)] = stack[bp+int(in.b)]
		case opStoreLocalSet:
			// update the pointee instead of replacing the pointer: free
			// variables may reference this local
			val := stack[bp+int(in.b)]
			lp := bp + int(in.a)
			if p, ok := stack[lp].(*ObjectPtr); ok {
				*p.Value = val
				val = p
			}
			stack[lp] = val
		case opStoreFree:
			*curFrame.freeVars[in.a].Value = stack[bp+int(in.b)]
		case opMove:
			var val Object
			if k, idx := in.b>>24, int(in.b&operandMask); k == kTemp {
				val = stack[bp+idx]
			} else if k == kConst {
				val = constants[idx]
			} else if k == kGlobal {
				val = globals[idx]
			} else if k == kLocal {
				val = stack[bp+idx]
				if p, ok := val.(*ObjectPtr); ok {
					val = *p.Value
				}
			} else {
				val = v.loadSlow(in.b, bp, curFrame)
			}
			if k, idx := in.a>>24, int(in.a&operandMask); k == kTemp || k == kLocal {
				stack[bp+idx] = val
			} else if k == kGlobal {
				globals[idx] = val
			} else {
				v.storeSlow(in.a, bp, curFrame, val)
			}
		case opBinary, opBinaryJF:
			var left Object
			if k, idx := in.b>>24, int(in.b&operandMask); k == kTemp {
				left = stack[bp+idx]
			} else if k == kConst {
				left = constants[idx]
			} else if k == kGlobal {
				left = globals[idx]
			} else if k == kLocal {
				left = stack[bp+idx]
				if p, ok := left.(*ObjectPtr); ok {
					left = *p.Value
				}
			} else {
				left = v.loadSlow(in.b, bp, curFrame)
			}
			var right Object
			if k, idx := in.c>>24, int(in.c&operandMask); k == kTemp {
				right = stack[bp+idx]
			} else if k == kConst {
				right = constants[idx]
			} else if k == kGlobal {
				right = globals[idx]
			} else if k == kLocal {
				right = stack[bp+idx]
				if p, ok := right.(*ObjectPtr); ok {
					right = *p.Value
				}
			} else {
				right = v.loadSlow(in.c, bp, curFrame)
			}
			tok := in.tok

			// fast paths for int op int and float op float, which is most
			// of what loops do; anything else goes through BinaryOp
			var res Object
			cmp := -1 // -1: not a fast-path comparison, else 0/1 = false/true
			switch l := left.(type) {
			case *Int:
				if r, ok := right.(*Int); ok {
					switch tok {
					case token.Add:
						res = v.newInt(l.Value + r.Value)
					case token.Sub:
						res = v.newInt(l.Value - r.Value)
					case token.Mul:
						res = v.newInt(l.Value * r.Value)
					case token.Quo:
						if r.Value != 0 {
							res = v.newInt(l.Value / r.Value)
						}
					case token.Rem:
						if r.Value != 0 {
							res = v.newInt(l.Value % r.Value)
						}
					case token.Less:
						cmp = 0
						if l.Value < r.Value {
							cmp = 1
						}
					case token.Greater:
						cmp = 0
						if l.Value > r.Value {
							cmp = 1
						}
					case token.LessEq:
						cmp = 0
						if l.Value <= r.Value {
							cmp = 1
						}
					case token.GreaterEq:
						cmp = 0
						if l.Value >= r.Value {
							cmp = 1
						}
					}
				}
			case *Float:
				if r, ok := right.(*Float); ok {
					switch tok {
					case token.Add:
						res = v.newFloat(l.Value + r.Value)
					case token.Sub:
						res = v.newFloat(l.Value - r.Value)
					case token.Mul:
						res = v.newFloat(l.Value * r.Value)
					case token.Quo:
						res = v.newFloat(l.Value / r.Value)
					case token.Less:
						cmp = 0
						if l.Value < r.Value {
							cmp = 1
						}
					case token.Greater:
						cmp = 0
						if l.Value > r.Value {
							cmp = 1
						}
					case token.LessEq:
						cmp = 0
						if l.Value <= r.Value {
							cmp = 1
						}
					case token.GreaterEq:
						cmp = 0
						if l.Value >= r.Value {
							cmp = 1
						}
					}
				}
			}
			if cmp >= 0 {
				allocs--
				if allocs == 0 {
					v.err = ErrObjectAllocLimit
					goto done
				}
				if in.op == opBinaryJF {
					if cmp == 0 {
						ip = int(in.a) - 1
					}
					continue
				}
				if cmp == 1 {
					res = TrueValue
				} else {
					res = FalseValue
				}
			} else {
				if res == nil {
					var e error
					res, e = left.BinaryOp(tok, right)
					if e != nil {
						if e == ErrInvalidOperator {
							v.err = fmt.Errorf("invalid operation: %s %s %s",
								left.TypeName(), tok.String(), right.TypeName())
							goto done
						}
						v.err = e
						goto done
					}
				}
				allocs--
				if allocs == 0 {
					v.err = ErrObjectAllocLimit
					goto done
				}
				if in.op == opBinaryJF {
					if res.IsFalsy() {
						ip = int(in.a) - 1
					}
					continue
				}
			}
			if k, idx := in.a>>24, int(in.a&operandMask); k == kTemp || k == kLocal {
				stack[bp+idx] = res
			} else if k == kGlobal {
				globals[idx] = res
			} else {
				v.storeSlow(in.a, bp, curFrame, res)
			}
		case opEqual, opNotEqual, opEqualJF, opNotEqualJF:
			var left Object
			if k, idx := in.b>>24, int(in.b&operandMask); k == kTemp {
				left = stack[bp+idx]
			} else if k == kConst {
				left = constants[idx]
			} else if k == kGlobal {
				left = globals[idx]
			} else if k == kLocal {
				left = stack[bp+idx]
				if p, ok := left.(*ObjectPtr); ok {
					left = *p.Value
				}
			} else {
				left = v.loadSlow(in.b, bp, curFrame)
			}
			var right Object
			if k, idx := in.c>>24, int(in.c&operandMask); k == kTemp {
				right = stack[bp+idx]
			} else if k == kConst {
				right = constants[idx]
			} else if k == kGlobal {
				right = globals[idx]
			} else if k == kLocal {
				right = stack[bp+idx]
				if p, ok := right.(*ObjectPtr); ok {
					right = *p.Value
				}
			} else {
				right = v.loadSlow(in.c, bp, curFrame)
			}
			var eq bool
			if l, ok := left.(*Int); ok {
				if r, ok := right.(*Int); ok {
					eq = l.Value == r.Value
				}
			} else {
				eq = left.Equals(right)
			}
			switch in.op {
			case opEqual, opNotEqual:
				var res Object = FalseValue
				if eq == (in.op == opEqual) {
					res = TrueValue
				}
				if k, idx := in.a>>24, int(in.a&operandMask); k == kTemp || k == kLocal {
					stack[bp+idx] = res
				} else if k == kGlobal {
					globals[idx] = res
				} else {
					v.storeSlow(in.a, bp, curFrame, res)
				}
			case opEqualJF:
				if !eq {
					ip = int(in.a) - 1
				}
			default: // opNotEqualJF
				if eq {
					ip = int(in.a) - 1
				}
			}
		case opLNot:
			if isFalsy(v.load(in.b, bp, curFrame)) {
				v.store(in.a, bp, curFrame, TrueValue)
			} else {
				v.store(in.a, bp, curFrame, FalseValue)
			}
		case opBComplement:
			operand := v.load(in.b, bp, curFrame)
			switch x := operand.(type) {
			case *Int:
				var res Object = v.newInt(^x.Value)
				allocs--
				if allocs == 0 {
					v.err = ErrObjectAllocLimit
					goto done
				}
				v.store(in.a, bp, curFrame, res)
			default:
				v.err = fmt.Errorf("invalid operation: ^%s",
					operand.TypeName())
				goto done
			}
		case opMinus:
			operand := v.load(in.b, bp, curFrame)
			var res Object
			switch x := operand.(type) {
			case *Int:
				res = v.newInt(-x.Value)
			case *Float:
				res = v.newFloat(-x.Value)
			default:
				v.err = fmt.Errorf("invalid operation: -%s",
					operand.TypeName())
				goto done
			}
			allocs--
			if allocs == 0 {
				v.err = ErrObjectAllocLimit
				goto done
			}
			v.store(in.a, bp, curFrame, res)
		case opJumpFalsy:
			var cond Object
			if k, idx := in.b>>24, int(in.b&operandMask); k == kTemp {
				cond = stack[bp+idx]
			} else if k == kConst {
				cond = constants[idx]
			} else if k == kGlobal {
				cond = globals[idx]
			} else if k == kLocal {
				cond = stack[bp+idx]
				if p, ok := cond.(*ObjectPtr); ok {
					cond = *p.Value
				}
			} else {
				cond = v.loadSlow(in.b, bp, curFrame)
			}
			if isFalsy(cond) {
				ip = int(in.a) - 1
			}
		case opAndJump:
			// the value stays in its slot on the jump path
			if isFalsy(stack[bp+int(in.b&operandMask)]) {
				ip = int(in.a) - 1
			}
		case opOrJump:
			if !isFalsy(stack[bp+int(in.b&operandMask)]) {
				ip = int(in.a) - 1
			}
		case opJump:
			if atomic.LoadInt64(&v.aborting) != 0 {
				goto done
			}
			ip = int(in.a) - 1
		case opSetSelGlobal, opSetSelLocal, opSetSelFree:
			// value at slot c, selectors right above it; they are read in
			// place from the operand stack, which nothing writes to before
			// indexAssign has returned
			vs := bp + int(in.c&operandMask)
			numSelectors := int(in.b)
			val := stack[vs]
			selectors := stack[vs+1 : vs+1+numSelectors]
			var dst Object
			switch in.op {
			case opSetSelGlobal:
				dst = v.globals[in.a]
			case opSetSelLocal:
				dst = stack[bp+int(in.a)]
				if obj, ok := dst.(*ObjectPtr); ok {
					dst = *obj.Value
				}
			default:
				dst = *curFrame.freeVars[in.a].Value
			}
			if e := indexAssign(dst, val, selectors); e != nil {
				v.err = e
				goto done
			}
		case opArray:
			base := bp + int(in.a&operandMask)
			numElements := int(in.b)
			var elements []Object
			if numElements > 0 {
				elements = make([]Object, numElements)
				copy(elements, stack[base:base+numElements])
			}
			var arr Object = &Array{Value: elements}
			allocs--
			if allocs == 0 {
				v.err = ErrObjectAllocLimit
				goto done
			}
			stack[base] = arr
		case opMap:
			base := bp + int(in.a&operandMask)
			numElements := int(in.b)
			kv := make(map[string]Object, numElements/2)
			for i := base; i < base+numElements; i += 2 {
				key := stack[i]
				value := stack[i+1]
				kv[key.(*String).Value] = value
			}
			var m Object = &Map{Value: kv}
			allocs--
			if allocs == 0 {
				v.err = ErrObjectAllocLimit
				goto done
			}
			stack[base] = m
		case opError:
			slot := bp + int(in.a&operandMask)
			var e Object = &Error{Value: stack[slot]}
			allocs--
			if allocs == 0 {
				v.err = ErrObjectAllocLimit
				goto done
			}
			stack[slot] = e
		case opImmutable:
			slot := bp + int(in.a&operandMask)
			switch value := stack[slot].(type) {
			case *Array:
				var immutableArray Object = &ImmutableArray{Value: value.Value}
				allocs--
				if allocs == 0 {
					v.err = ErrObjectAllocLimit
					goto done
				}
				stack[slot] = immutableArray
			case *Map:
				var immutableMap Object = &ImmutableMap{Value: value.Value}
				allocs--
				if allocs == 0 {
					v.err = ErrObjectAllocLimit
					goto done
				}
				stack[slot] = immutableMap
			}
		case opIndex:
			var left Object
			if k, idx := in.b>>24, int(in.b&operandMask); k == kTemp {
				left = stack[bp+idx]
			} else if k == kConst {
				left = constants[idx]
			} else if k == kGlobal {
				left = globals[idx]
			} else if k == kLocal {
				left = stack[bp+idx]
				if p, ok := left.(*ObjectPtr); ok {
					left = *p.Value
				}
			} else {
				left = v.loadSlow(in.b, bp, curFrame)
			}
			var index Object
			if k, idx := in.c>>24, int(in.c&operandMask); k == kTemp {
				index = stack[bp+idx]
			} else if k == kConst {
				index = constants[idx]
			} else if k == kGlobal {
				index = globals[idx]
			} else if k == kLocal {
				index = stack[bp+idx]
				if p, ok := index.(*ObjectPtr); ok {
					index = *p.Value
				}
			} else {
				index = v.loadSlow(in.c, bp, curFrame)
			}
			val, err := left.IndexGet(index)
			if err != nil {
				if err == ErrNotIndexable {
					v.err = fmt.Errorf("not indexable: %s", index.TypeName())
					goto done
				}
				if err == ErrInvalidIndexType {
					v.err = fmt.Errorf("invalid index type: %s",
						index.TypeName())
					goto done
				}
				v.err = err
				goto done
			}
			if val == nil {
				val = UndefinedValue
			}
			if k, idx := in.a>>24, int(in.a&operandMask); k == kTemp || k == kLocal {
				stack[bp+idx] = val
			} else if k == kGlobal {
				globals[idx] = val
			} else {
				v.storeSlow(in.a, bp, curFrame, val)
			}
		case opSliceIndex:
			slot := bp + int(in.a&operandMask)
			left := stack[slot]
			low := stack[slot+1]
			high := stack[slot+2]

			var lowIdx int64
			if low != UndefinedValue {
				if lowInt, ok := low.(*Int); ok {
					lowIdx = lowInt.Value
				} else {
					v.err = fmt.Errorf("invalid slice index type: %s",
						low.TypeName())
					goto done
				}
			}

			var numElements int64
			switch left := left.(type) {
			case *Array:
				numElements = int64(len(left.Value))
			case *ImmutableArray:
				numElements = int64(len(left.Value))
			case *String:
				numElements = int64(len(left.Value))
			case *Bytes:
				numElements = int64(len(left.Value))
			default:
				v.err = fmt.Errorf("not indexable: %s", left.TypeName())
				goto done
			}

			var highIdx int64
			if high == UndefinedValue {
				highIdx = numElements
			} else if highInt, ok := high.(*Int); ok {
				highIdx = highInt.Value
			} else {
				v.err = fmt.Errorf("invalid slice index type: %s",
					high.TypeName())
				goto done
			}
			if lowIdx > highIdx {
				v.err = fmt.Errorf("invalid slice index: %d > %d",
					lowIdx, highIdx)
				goto done
			}
			if lowIdx < 0 {
				lowIdx = 0
			} else if lowIdx > numElements {
				lowIdx = numElements
			}
			if highIdx < 0 {
				highIdx = 0
			} else if highIdx > numElements {
				highIdx = numElements
			}

			var val Object
			switch left := left.(type) {
			case *Array:
				val = &Array{Value: left.Value[lowIdx:highIdx]}
			case *ImmutableArray:
				val = &Array{Value: left.Value[lowIdx:highIdx]}
			case *String:
				val = &String{Value: left.Value[lowIdx:highIdx]}
			case *Bytes:
				val = &Bytes{Value: left.Value[lowIdx:highIdx]}
			}
			allocs--
			if allocs == 0 {
				v.err = ErrObjectAllocLimit
				goto done
			}
			stack[slot] = val
		case opCall:
			if atomic.LoadInt64(&v.aborting) != 0 {
				goto done
			}
			base := bp + int(in.a&operandMask) // callee; args follow
			numArgs := int(in.b)
			spread := in.c

			value := stack[base]
			switch value.(type) {
			case *CompiledFunction, *BuiltinFunction, *UserFunction:
			default:
				if !value.CanCall() {
					v.err = fmt.Errorf("not callable: %s", value.TypeName())
					goto done
				}
			}

			if spread == 1 {
				last := base + numArgs
				var items []Object
				switch arr := stack[last].(type) {
				case *Array:
					items = arr.Value
				case *ImmutableArray:
					items = arr.Value
				default:
					v.err = fmt.Errorf("not an array: %s", arr.TypeName())
					goto done
				}
				if last+len(items) > StackSize {
					v.err = ErrStackOverflow
					goto done
				}
				copy(stack[last:], items)
				numArgs += len(items) - 1
			}

			if callee, ok := value.(*CompiledFunction); ok {
				if callee.IsModule {
					if cached, ok := v.modules[callee]; ok {
						stack[base] = cached
						continue
					}
				}
				if callee.VarArgs {
					// if the closure is variadic,
					// roll up all variadic parameters into an array
					realArgs := callee.NumParameters - 1
					varArgs := numArgs - realArgs
					if varArgs >= 0 {
						numArgs = realArgs + 1
						args := make([]Object, varArgs)
						spStart := base + 1 + realArgs
						copy(args, stack[spStart:spStart+varArgs])
						stack[spStart] = &Array{Value: args}
					}
				}
				if numArgs != callee.NumParameters {
					if callee.VarArgs {
						v.err = fmt.Errorf(
							"wrong number of arguments: want>=%d, got=%d",
							callee.NumParameters-1, numArgs)
					} else {
						v.err = fmt.Errorf(
							"wrong number of arguments: want=%d, got=%d",
							callee.NumParameters, numArgs)
					}
					goto done
				}

				// test if it's tail-call
				if callee == curFrame.fn { // recursion
					next := &code[ip+1]
					tail := next.op == opReturn && next.b == in.a
					if !tail && next.op == opReturn0 {
						// the result is dropped and the function returns
						// undefined; remember that for the final return
						tail = true
						curFrame.discard = true
					}
					if tail {
						copy(stack[bp:bp+numArgs], stack[base+1:base+1+numArgs])
						ip = -1 // reset IP to beginning of the frame
						continue
					}
				}
				nbp := base + 1
				if framesIndex >= MaxFrames ||
					nbp+callee.NumLocals+callee.stackDepth > StackSize {
					v.err = ErrStackOverflow
					goto done
				}

				// update call frame
				curFrame.ip = ip // store current ip before call
				curFrame = &(v.frames[framesIndex])
				curFrame.fn = callee
				curFrame.freeVars = callee.Free
				curFrame.basePointer = nbp
				curFrame.discard = false
				code = callee.code
				bp = nbp
				ip = -1
				framesIndex++
				if framesIndex > v.framesHigh {
					v.framesHigh = framesIndex
				}
			} else {
				var ret Object
				var e error
				args := stack[base+1 : base+1+numArgs]
				switch fn := value.(type) {
				case *BuiltinFunction:
					if fn.stackArgs {
						// the builtin does not retain args, and nothing
						// touches the operand stack until it returns
						ret, e = fn.Value(args...)
					} else {
						ret, e = fn.Value(append([]Object(nil), args...)...)
					}
				case *UserFunction:
					if fn.StackArgs {
						ret, e = fn.Value(args...)
					} else {
						ret, e = fn.Value(append([]Object(nil), args...)...)
					}
				default:
					ret, e = value.Call(append([]Object(nil), args...)...)
				}

				// runtime error
				if e != nil {
					if e == ErrWrongNumArguments {
						v.err = fmt.Errorf(
							"wrong number of arguments in call to '%s'",
							value.TypeName())
						goto done
					}
					if e, ok := e.(ErrInvalidArgumentType); ok {
						v.err = fmt.Errorf(
							"invalid type for argument '%s' in call to '%s': "+
								"expected %s, found %s",
							e.Name, value.TypeName(), e.Expected, e.Found)
						goto done
					}
					v.err = e
					goto done
				}

				// nil return -> undefined
				if ret == nil {
					ret = UndefinedValue
				}
				allocs--
				if allocs == 0 {
					v.err = ErrObjectAllocLimit
					goto done
				}
				stack[base] = ret
			}
		case opReturn, opReturn0:
			var retVal Object = UndefinedValue
			if in.op == opReturn && !curFrame.discard {
				var val Object
				if k, idx := in.b>>24, int(in.b&operandMask); k == kTemp {
					val = stack[bp+idx]
				} else if k == kConst {
					val = constants[idx]
				} else if k == kGlobal {
					val = globals[idx]
				} else if k == kLocal {
					val = stack[bp+idx]
					if p, ok := val.(*ObjectPtr); ok {
						val = *p.Value
					}
				} else {
					val = v.loadSlow(in.b, bp, curFrame)
				}
				retVal = val
			}
			if fn := curFrame.fn; fn.IsModule {
				if v.modules == nil {
					v.modules = make(map[*CompiledFunction]Object)
				}
				v.modules[fn] = retVal
			}
			framesIndex--
			// the result replaces the callee in the caller's frame
			stack[bp-1] = retVal
			curFrame = &v.frames[framesIndex-1]
			code = curFrame.fn.code
			bp = curFrame.basePointer
			ip = curFrame.ip
		case opGetLocalPtr:
			lp := bp + int(in.b)
			val := stack[lp]
			var freeVar *ObjectPtr
			if obj, ok := val.(*ObjectPtr); ok {
				freeVar = obj
			} else {
				freeVar = &ObjectPtr{Value: &val}
				stack[lp] = freeVar
			}
			stack[bp+int(in.a&operandMask)] = freeVar
		case opClosure:
			base := bp + int(in.a&operandMask) // free vars start here
			numFree := int(in.c)
			fn, ok := v.constants[in.b].(*CompiledFunction)
			if !ok {
				v.err = fmt.Errorf("not function: %s", fn.TypeName())
				goto done
			}
			free := make([]*ObjectPtr, numFree)
			for i := 0; i < numFree; i++ {
				switch freeVar := (stack[base+i]).(type) {
				case *ObjectPtr:
					free[i] = freeVar
				default:
					free[i] = &ObjectPtr{
						Value: &stack[base+i],
					}
				}
			}
			cl := &CompiledFunction{
				Instructions:  fn.Instructions,
				NumLocals:     fn.NumLocals,
				NumParameters: fn.NumParameters,
				VarArgs:       fn.VarArgs,
				SourceMap:     fn.SourceMap,
				stackDepth:    fn.stackDepth,
				code:          codeFor(fn),
				Free:          free,
			}
			allocs--
			if allocs == 0 {
				v.err = ErrObjectAllocLimit
				goto done
			}
			stack[base] = cl
		case opIteratorInit:
			slot := bp + int(in.a&operandMask)
			dst := stack[slot]
			if !dst.CanIterate() {
				v.err = fmt.Errorf("not iterable: %s", dst.TypeName())
				goto done
			}
			var iterator Object = dst.Iterate()
			allocs--
			if allocs == 0 {
				v.err = ErrObjectAllocLimit
				goto done
			}
			stack[slot] = iterator
		case opIteratorNext:
			slot := bp + int(in.a&operandMask)
			if stack[slot].(Iterator).Next() {
				stack[slot] = TrueValue
			} else {
				stack[slot] = FalseValue
			}
		case opIteratorKey:
			slot := bp + int(in.a&operandMask)
			stack[slot] = stack[slot].(Iterator).Key()
		case opIteratorValue:
			slot := bp + int(in.a&operandMask)
			stack[slot] = stack[slot].(Iterator).Value()
		case opSuspend:
			v.sp = bp + int(in.a)
			goto done
		default:
			v.err = fmt.Errorf("unknown opcode: %d", in.a)
			goto done
		}
	}

done:
	v.ip = ip
	v.curFrame = curFrame
	v.framesIndex = framesIndex
	v.allocs = allocs
}

// isFalsy reports whether o is falsy, short-circuiting the two shared Bool
// values so the common case needs no dynamic dispatch.
func isFalsy(o Object) bool {
	if b, ok := o.(*Bool); ok {
		return !b.value
	}
	return o.IsFalsy()
}

// IsStackEmpty tests if the stack is empty or not.
func (v *VM) IsStackEmpty() bool {
	return v.sp == 0
}

func indexAssign(dst, src Object, selectors []Object) error {
	numSel := len(selectors)
	for sidx := numSel - 1; sidx > 0; sidx-- {
		next, err := dst.IndexGet(selectors[sidx])
		if err != nil {
			if err == ErrNotIndexable {
				return fmt.Errorf("not indexable: %s", dst.TypeName())
			}
			if err == ErrInvalidIndexType {
				return fmt.Errorf("invalid index type: %s",
					selectors[sidx].TypeName())
			}
			return err
		}
		dst = next
	}

	if err := dst.IndexSet(selectors[0], src); err != nil {
		if err == ErrNotIndexAssignable {
			return fmt.Errorf("not index-assignable: %s", dst.TypeName())
		}
		if err == ErrInvalidIndexValueType {
			return fmt.Errorf("invaid index value type: %s", src.TypeName())
		}
		return err
	}
	return nil
}
