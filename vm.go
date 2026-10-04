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
	ip          int
	basePointer int
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
	curFrame    *frame
	curInsts    []byte
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
	if globals == nil {
		globals = make([]Object, GlobalsSize)
	}
	v := &VM{
		constants:   bytecode.Constants,
		sp:          0,
		globals:     globals,
		fileSet:     bytecode.FileSet,
		framesIndex: 1,
		ip:          -1,
		maxAllocs:   maxAllocs,
	}
	v.frames[0].fn = bytecode.MainFunction
	v.frames[0].ip = -1
	v.curFrame = &v.frames[0]
	v.curInsts = v.curFrame.fn.Instructions
	return v
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
	v.curInsts = v.curFrame.fn.Instructions
	v.framesIndex = 1
	v.ip = -1
	v.allocs = v.maxAllocs + 1
	v.modules = nil
	v.err = nil

	v.run()
	atomic.StoreInt64(&v.aborting, 0)
	err = v.err
	if err != nil {
		filePos := v.fileSet.Position(
			v.curFrame.fn.SourcePos(v.ip - 1))
		err = fmt.Errorf("Runtime Error: %w\n\tat %s",
			err, filePos)
		for v.framesIndex > 1 {
			v.framesIndex--
			v.curFrame = &v.frames[v.framesIndex-1]
			filePos = v.fileSet.Position(
				v.curFrame.fn.SourcePos(v.curFrame.ip - 1))
			err = fmt.Errorf("%w\n\tat %s", err, filePos)
		}
		return err
	}
	return nil
}

// run is the interpreter loop. The hot VM state (instruction pointer, stack
// pointer, current instructions and frame) lives in locals so the compiler can
// keep it in registers instead of reloading it from *VM after every store or
// call; it is written back to v on exit so error reporting sees it.
func (v *VM) run() {
	var (
		ip          = v.ip
		sp          = v.sp
		insts       = v.curInsts
		stack       = &v.stack
		constants   = v.constants
		globals     = v.globals
		curFrame    = v.curFrame
		framesIndex = v.framesIndex
		allocs      = v.allocs
	)

	// The abort flag is checked only on jumps and calls: any non-terminating
	// execution must repeatedly take a backward jump or perform a call, so this
	// is enough to stop it, and it keeps the atomic load off the straight-line
	// dispatch path.
	for {
		ip++

		switch insts[ip] {
		case parser.OpConstant:
			ip += 2
			cidx := int(insts[ip]) | int(insts[ip-1])<<8

			stack[sp] = constants[cidx]
			sp++
		case parser.OpNull:
			stack[sp] = UndefinedValue
			sp++
		case parser.OpBinaryOp:
			ip++
			right := stack[sp-1]
			left := stack[sp-2]
			tok := token.Token(insts[ip])

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
				// a comparison feeding a conditional jump does not need to
				// materialize the bool on the stack
				if ip+1 < len(insts) && insts[ip+1] == parser.OpJumpFalsy {
					sp -= 2
					ip += 5
					allocs--
					if allocs == 0 {
						v.err = ErrObjectAllocLimit
						goto done
					}
					if cmp == 0 {
						pos := int(insts[ip]) | int(insts[ip-1])<<8 | int(insts[ip-2])<<16 | int(insts[ip-3])<<24
						ip = pos - 1
					}
					continue
				}
				if cmp == 1 {
					res = TrueValue
				} else {
					res = FalseValue
				}
			}
			if res == nil {
				var e error
				res, e = left.BinaryOp(tok, right)
				if e != nil {
					sp -= 2
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

			stack[sp-2] = res
			sp--
		case parser.OpEqual:
			right := stack[sp-1]
			left := stack[sp-2]
			sp -= 2
			var eq bool
			if l, ok := left.(*Int); ok {
				if r, ok := right.(*Int); ok {
					eq = l.Value == r.Value
				}
			} else {
				eq = left.Equals(right)
			}
			if eq {
				stack[sp] = TrueValue
			} else {
				stack[sp] = FalseValue
			}
			sp++
		case parser.OpNotEqual:
			right := stack[sp-1]
			left := stack[sp-2]
			sp -= 2
			var eq bool
			if l, ok := left.(*Int); ok {
				if r, ok := right.(*Int); ok {
					eq = l.Value == r.Value
				}
			} else {
				eq = left.Equals(right)
			}
			if eq {
				stack[sp] = FalseValue
			} else {
				stack[sp] = TrueValue
			}
			sp++
		case parser.OpPop:
			sp--
		case parser.OpTrue:
			stack[sp] = TrueValue
			sp++
		case parser.OpFalse:
			stack[sp] = FalseValue
			sp++
		case parser.OpLNot:
			operand := stack[sp-1]
			if isFalsy(operand) {
				stack[sp-1] = TrueValue
			} else {
				stack[sp-1] = FalseValue
			}
		case parser.OpBComplement:
			operand := stack[sp-1]
			sp--

			switch x := operand.(type) {
			case *Int:
				var res Object = v.newInt(^x.Value)
				allocs--
				if allocs == 0 {
					v.err = ErrObjectAllocLimit
					goto done
				}
				stack[sp] = res
				sp++
			default:
				v.err = fmt.Errorf("invalid operation: ^%s",
					operand.TypeName())
				goto done
			}
		case parser.OpMinus:
			operand := stack[sp-1]
			sp--

			switch x := operand.(type) {
			case *Int:
				var res Object = v.newInt(-x.Value)
				allocs--
				if allocs == 0 {
					v.err = ErrObjectAllocLimit
					goto done
				}
				stack[sp] = res
				sp++
			case *Float:
				var res Object = v.newFloat(-x.Value)
				allocs--
				if allocs == 0 {
					v.err = ErrObjectAllocLimit
					goto done
				}
				stack[sp] = res
				sp++
			default:
				v.err = fmt.Errorf("invalid operation: -%s",
					operand.TypeName())
				goto done
			}
		case parser.OpJumpFalsy:
			ip += 4
			sp--
			if isFalsy(stack[sp]) {
				pos := int(insts[ip]) | int(insts[ip-1])<<8 | int(insts[ip-2])<<16 | int(insts[ip-3])<<24
				ip = pos - 1
			}
		case parser.OpAndJump:
			ip += 4
			if isFalsy(stack[sp-1]) {
				pos := int(insts[ip]) | int(insts[ip-1])<<8 | int(insts[ip-2])<<16 | int(insts[ip-3])<<24
				ip = pos - 1
			} else {
				sp--
			}
		case parser.OpOrJump:
			ip += 4
			if isFalsy(stack[sp-1]) {
				sp--
			} else {
				pos := int(insts[ip]) | int(insts[ip-1])<<8 | int(insts[ip-2])<<16 | int(insts[ip-3])<<24
				ip = pos - 1
			}
		case parser.OpJump:
			if atomic.LoadInt64(&v.aborting) != 0 {
				goto done
			}
			pos := int(insts[ip+4]) | int(insts[ip+3])<<8 | int(insts[ip+2])<<16 | int(insts[ip+1])<<24
			ip = pos - 1
		case parser.OpSetGlobal:
			ip += 2
			sp--
			globalIndex := int(insts[ip]) | int(insts[ip-1])<<8
			globals[globalIndex] = stack[sp]
		case parser.OpSetSelGlobal:
			ip += 3
			globalIndex := int(insts[ip-1]) | int(insts[ip-2])<<8
			numSelectors := int(insts[ip])

			// selectors and RHS value
			selectors := make([]Object, numSelectors)
			for i := 0; i < numSelectors; i++ {
				selectors[i] = stack[sp-numSelectors+i]
			}
			val := stack[sp-numSelectors-1]
			sp -= numSelectors + 1
			e := indexAssign(globals[globalIndex], val, selectors)
			if e != nil {
				v.err = e
				goto done
			}
		case parser.OpGetGlobal:
			ip += 2
			globalIndex := int(insts[ip]) | int(insts[ip-1])<<8
			val := globals[globalIndex]
			stack[sp] = val
			sp++
		case parser.OpArray:
			ip += 2
			numElements := int(insts[ip]) | int(insts[ip-1])<<8

			var elements []Object
			for i := sp - numElements; i < sp; i++ {
				elements = append(elements, stack[i])
			}
			sp -= numElements

			var arr Object = &Array{Value: elements}
			allocs--
			if allocs == 0 {
				v.err = ErrObjectAllocLimit
				goto done
			}

			stack[sp] = arr
			sp++
		case parser.OpMap:
			ip += 2
			numElements := int(insts[ip]) | int(insts[ip-1])<<8
			kv := make(map[string]Object, numElements)
			for i := sp - numElements; i < sp; i += 2 {
				key := stack[i]
				value := stack[i+1]
				kv[key.(*String).Value] = value
			}
			sp -= numElements

			var m Object = &Map{Value: kv}
			allocs--
			if allocs == 0 {
				v.err = ErrObjectAllocLimit
				goto done
			}
			stack[sp] = m
			sp++
		case parser.OpError:
			value := stack[sp-1]
			var e Object = &Error{
				Value: value,
			}
			allocs--
			if allocs == 0 {
				v.err = ErrObjectAllocLimit
				goto done
			}
			stack[sp-1] = e
		case parser.OpImmutable:
			value := stack[sp-1]
			switch value := value.(type) {
			case *Array:
				var immutableArray Object = &ImmutableArray{
					Value: value.Value,
				}
				allocs--
				if allocs == 0 {
					v.err = ErrObjectAllocLimit
					goto done
				}
				stack[sp-1] = immutableArray
			case *Map:
				var immutableMap Object = &ImmutableMap{
					Value: value.Value,
				}
				allocs--
				if allocs == 0 {
					v.err = ErrObjectAllocLimit
					goto done
				}
				stack[sp-1] = immutableMap
			}
		case parser.OpIndex:
			index := stack[sp-1]
			left := stack[sp-2]
			sp -= 2

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
			stack[sp] = val
			sp++
		case parser.OpSliceIndex:
			high := stack[sp-1]
			low := stack[sp-2]
			left := stack[sp-3]
			sp -= 3

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
			stack[sp] = val
			sp++
		case parser.OpCall:
			if atomic.LoadInt64(&v.aborting) != 0 {
				goto done
			}
			numArgs := int(insts[ip+1])
			spread := int(insts[ip+2])
			ip += 2

			value := stack[sp-1-numArgs]
			switch value.(type) {
			case *CompiledFunction, *BuiltinFunction, *UserFunction:
			default:
				if !value.CanCall() {
					v.err = fmt.Errorf("not callable: %s", value.TypeName())
					goto done
				}
			}

			if spread == 1 {
				sp--
				var items []Object
				switch arr := stack[sp].(type) {
				case *Array:
					items = arr.Value
				case *ImmutableArray:
					items = arr.Value
				default:
					v.err = fmt.Errorf("not an array: %s", arr.TypeName())
					goto done
				}
				copy(stack[sp:], items)
				sp += len(items)
				numArgs += len(items) - 1
			}

			if callee, ok := value.(*CompiledFunction); ok {
				if callee.IsModule {
					if cached, ok := v.modules[callee]; ok {
						sp -= numArgs + 1
						stack[sp] = cached
						sp++
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
						spStart := sp - varArgs
						copy(args, stack[spStart:sp])
						stack[spStart] = &Array{Value: args}
						sp = spStart + 1
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
					nextOp := insts[ip+1]
					if nextOp == parser.OpReturn ||
						(nextOp == parser.OpPop &&
							parser.OpReturn == insts[ip+2]) {
						copy(stack[curFrame.basePointer:curFrame.basePointer+numArgs],
							stack[sp-numArgs:sp])
						sp -= numArgs + 1
						ip = -1 // reset IP to beginning of the frame
						continue
					}
				}
				if framesIndex >= MaxFrames {
					v.err = ErrStackOverflow
					goto done
				}

				// update call frame
				curFrame.ip = ip // store current ip before call
				curFrame = &(v.frames[framesIndex])
				curFrame.fn = callee
				curFrame.freeVars = callee.Free
				curFrame.basePointer = sp - numArgs
				insts = callee.Instructions
				ip = -1
				framesIndex++
				sp = sp - numArgs + callee.NumLocals
			} else {
				var ret Object
				var e error
				switch fn := value.(type) {
				case *BuiltinFunction:
					if fn.stackArgs {
						// the builtin does not retain args, and nothing
						// touches the operand stack until it returns
						ret, e = fn.Value(stack[sp-numArgs : sp]...)
					} else {
						args := make([]Object, numArgs)
						copy(args, stack[sp-numArgs:sp])
						ret, e = fn.Value(args...)
					}
				case *UserFunction:
					args := make([]Object, numArgs)
					copy(args, stack[sp-numArgs:sp])
					ret, e = fn.Value(args...)
				default:
					args := make([]Object, numArgs)
					copy(args, stack[sp-numArgs:sp])
					ret, e = value.Call(args...)
				}
				sp -= numArgs + 1

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
				stack[sp] = ret
				sp++
			}
		case parser.OpReturn:
			ip++
			var retVal Object
			if int(insts[ip]) == 1 {
				retVal = stack[sp-1]
			} else {
				retVal = UndefinedValue
			}
			if fn := curFrame.fn; fn.IsModule {
				if v.modules == nil {
					v.modules = make(map[*CompiledFunction]Object)
				}
				v.modules[fn] = retVal
			}
			framesIndex--
			curFrame = &v.frames[framesIndex-1]
			insts = curFrame.fn.Instructions
			ip = curFrame.ip
			sp = v.frames[framesIndex].basePointer
			// skip stack overflow check because (newSP) <= (oldSP)
			stack[sp-1] = retVal
		case parser.OpDefineLocal:
			ip++
			localIndex := int(insts[ip])

			// local variables can be mutated by other actions
			// so always store the copy of popped value
			sp--
			stack[curFrame.basePointer+localIndex] = stack[sp]
		case parser.OpSetLocal:
			localIndex := int(insts[ip+1])
			ip++
			lp := curFrame.basePointer + localIndex

			// update pointee of stack[lp] instead of replacing the pointer
			// itself. this is needed because there can be free variables
			// referencing the same local variables.
			val := stack[sp-1]
			sp--
			if obj, ok := stack[lp].(*ObjectPtr); ok {
				*obj.Value = val
				val = obj
			}
			stack[lp] = val // also use a copy of popped value
		case parser.OpSetSelLocal:
			localIndex := int(insts[ip+1])
			numSelectors := int(insts[ip+2])
			ip += 2

			// selectors and RHS value
			selectors := make([]Object, numSelectors)
			for i := 0; i < numSelectors; i++ {
				selectors[i] = stack[sp-numSelectors+i]
			}
			val := stack[sp-numSelectors-1]
			sp -= numSelectors + 1
			dst := stack[curFrame.basePointer+localIndex]
			if obj, ok := dst.(*ObjectPtr); ok {
				dst = *obj.Value
			}
			e := indexAssign(dst, val, selectors)
			if e != nil {
				v.err = e
				goto done
			}
		case parser.OpGetLocal:
			ip++
			localIndex := int(insts[ip])
			val := stack[curFrame.basePointer+localIndex]
			if obj, ok := val.(*ObjectPtr); ok {
				val = *obj.Value
			}
			stack[sp] = val
			sp++
		case parser.OpGetBuiltin:
			ip++
			builtinIndex := int(insts[ip])
			stack[sp] = builtinFuncs[builtinIndex]
			sp++
		case parser.OpClosure:
			ip += 3
			constIndex := int(insts[ip-1]) | int(insts[ip-2])<<8
			numFree := int(insts[ip])
			fn, ok := constants[constIndex].(*CompiledFunction)
			if !ok {
				v.err = fmt.Errorf("not function: %s", fn.TypeName())
				goto done
			}
			free := make([]*ObjectPtr, numFree)
			for i := 0; i < numFree; i++ {
				switch freeVar := (stack[sp-numFree+i]).(type) {
				case *ObjectPtr:
					free[i] = freeVar
				default:
					free[i] = &ObjectPtr{
						Value: &stack[sp-numFree+i],
					}
				}
			}
			sp -= numFree
			cl := &CompiledFunction{
				Instructions:  fn.Instructions,
				NumLocals:     fn.NumLocals,
				NumParameters: fn.NumParameters,
				VarArgs:       fn.VarArgs,
				SourceMap:     fn.SourceMap,
				Free:          free,
			}
			allocs--
			if allocs == 0 {
				v.err = ErrObjectAllocLimit
				goto done
			}
			stack[sp] = cl
			sp++
		case parser.OpGetFreePtr:
			ip++
			freeIndex := int(insts[ip])
			val := curFrame.freeVars[freeIndex]
			stack[sp] = val
			sp++
		case parser.OpGetFree:
			ip++
			freeIndex := int(insts[ip])
			val := *curFrame.freeVars[freeIndex].Value
			stack[sp] = val
			sp++
		case parser.OpSetFree:
			ip++
			freeIndex := int(insts[ip])
			*curFrame.freeVars[freeIndex].Value = stack[sp-1]
			sp--
		case parser.OpGetLocalPtr:
			ip++
			localIndex := int(insts[ip])
			lp := curFrame.basePointer + localIndex
			val := stack[lp]
			var freeVar *ObjectPtr
			if obj, ok := val.(*ObjectPtr); ok {
				freeVar = obj
			} else {
				freeVar = &ObjectPtr{Value: &val}
				stack[lp] = freeVar
			}
			stack[sp] = freeVar
			sp++
		case parser.OpSetSelFree:
			ip += 2
			freeIndex := int(insts[ip-1])
			numSelectors := int(insts[ip])

			// selectors and RHS value
			selectors := make([]Object, numSelectors)
			for i := 0; i < numSelectors; i++ {
				selectors[i] = stack[sp-numSelectors+i]
			}
			val := stack[sp-numSelectors-1]
			sp -= numSelectors + 1
			e := indexAssign(*curFrame.freeVars[freeIndex].Value,
				val, selectors)
			if e != nil {
				v.err = e
				goto done
			}
		case parser.OpIteratorInit:
			var iterator Object
			dst := stack[sp-1]
			sp--
			if !dst.CanIterate() {
				v.err = fmt.Errorf("not iterable: %s", dst.TypeName())
				goto done
			}
			iterator = dst.Iterate()
			allocs--
			if allocs == 0 {
				v.err = ErrObjectAllocLimit
				goto done
			}
			stack[sp] = iterator
			sp++
		case parser.OpIteratorNext:
			iterator := stack[sp-1]
			if iterator.(Iterator).Next() {
				stack[sp-1] = TrueValue
			} else {
				stack[sp-1] = FalseValue
			}
		case parser.OpIteratorKey:
			iterator := stack[sp-1]
			stack[sp-1] = iterator.(Iterator).Key()
		case parser.OpIteratorValue:
			iterator := stack[sp-1]
			stack[sp-1] = iterator.(Iterator).Value()
		case parser.OpSuspend:
			goto done
		default:
			v.err = fmt.Errorf("unknown opcode: %d", insts[ip])
			goto done
		}
	}

done:
	v.ip = ip
	v.sp = sp
	v.curInsts = insts
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
