package tengo_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/d5/tengo/v2"
	"github.com/d5/tengo/v2/parser"
	"github.com/d5/tengo/v2/require"
)

type srcfile struct {
	name string
	size int
}

func TestBytecode(t *testing.T) {
	testBytecodeSerialization(t, bytecode(concatInsts(), objectsArray()))

	testBytecodeSerialization(t, bytecode(
		concatInsts(), objectsArray(
			&tengo.Char{Value: 'y'},
			&tengo.Float{Value: 93.11},
			compiledFunction(1, 0,
				tengo.MakeInstruction(parser.OpConstant, 3),
				tengo.MakeInstruction(parser.OpSetLocal, 0),
				tengo.MakeInstruction(parser.OpGetGlobal, 0),
				tengo.MakeInstruction(parser.OpGetFree, 0)),
			&tengo.Float{Value: 39.2},
			&tengo.Int{Value: 192},
			&tengo.String{Value: "bar"})))

	testBytecodeSerialization(t, bytecodeFileSet(
		concatInsts(
			tengo.MakeInstruction(parser.OpConstant, 0),
			tengo.MakeInstruction(parser.OpSetGlobal, 0),
			tengo.MakeInstruction(parser.OpConstant, 6),
			tengo.MakeInstruction(parser.OpPop)),
		objectsArray(
			&tengo.Int{Value: 55},
			&tengo.Int{Value: 66},
			&tengo.Int{Value: 77},
			&tengo.Int{Value: 88},
			&tengo.ImmutableMap{
				Value: map[string]tengo.Object{
					"array": &tengo.ImmutableArray{
						Value: []tengo.Object{
							&tengo.Int{Value: 1},
							&tengo.Int{Value: 2},
							&tengo.Int{Value: 3},
							tengo.TrueValue,
							tengo.FalseValue,
							tengo.UndefinedValue,
						},
					},
					"true":  tengo.TrueValue,
					"false": tengo.FalseValue,
					"bytes": &tengo.Bytes{Value: make([]byte, 16)},
					"char":  &tengo.Char{Value: 'Y'},
					"error": &tengo.Error{Value: &tengo.String{
						Value: "some error",
					}},
					"float": &tengo.Float{Value: -19.84},
					"immutable_array": &tengo.ImmutableArray{
						Value: []tengo.Object{
							&tengo.Int{Value: 1},
							&tengo.Int{Value: 2},
							&tengo.Int{Value: 3},
							tengo.TrueValue,
							tengo.FalseValue,
							tengo.UndefinedValue,
						},
					},
					"immutable_map": &tengo.ImmutableMap{
						Value: map[string]tengo.Object{
							"a": &tengo.Int{Value: 1},
							"b": &tengo.Int{Value: 2},
							"c": &tengo.Int{Value: 3},
							"d": tengo.TrueValue,
							"e": tengo.FalseValue,
							"f": tengo.UndefinedValue,
						},
					},
					"int": &tengo.Int{Value: 91},
					"map": &tengo.Map{
						Value: map[string]tengo.Object{
							"a": &tengo.Int{Value: 1},
							"b": &tengo.Int{Value: 2},
							"c": &tengo.Int{Value: 3},
							"d": tengo.TrueValue,
							"e": tengo.FalseValue,
							"f": tengo.UndefinedValue,
						},
					},
					"string":    &tengo.String{Value: "foo bar"},
					"time":      &tengo.Time{Value: time.Now()},
					"undefined": tengo.UndefinedValue,
				},
			},
			compiledFunction(1, 0,
				tengo.MakeInstruction(parser.OpConstant, 3),
				tengo.MakeInstruction(parser.OpSetLocal, 0),
				tengo.MakeInstruction(parser.OpGetGlobal, 0),
				tengo.MakeInstruction(parser.OpGetFree, 0),
				tengo.MakeInstruction(parser.OpBinaryOp, 11),
				tengo.MakeInstruction(parser.OpGetFree, 1),
				tengo.MakeInstruction(parser.OpBinaryOp, 11),
				tengo.MakeInstruction(parser.OpGetLocal, 0),
				tengo.MakeInstruction(parser.OpBinaryOp, 11),
				tengo.MakeInstruction(parser.OpReturn, 1)),
			compiledFunction(1, 0,
				tengo.MakeInstruction(parser.OpConstant, 2),
				tengo.MakeInstruction(parser.OpSetLocal, 0),
				tengo.MakeInstruction(parser.OpGetFree, 0),
				tengo.MakeInstruction(parser.OpGetLocal, 0),
				tengo.MakeInstruction(parser.OpClosure, 4, 2),
				tengo.MakeInstruction(parser.OpReturn, 1)),
			compiledFunction(1, 0,
				tengo.MakeInstruction(parser.OpConstant, 1),
				tengo.MakeInstruction(parser.OpSetLocal, 0),
				tengo.MakeInstruction(parser.OpGetLocal, 0),
				tengo.MakeInstruction(parser.OpClosure, 5, 1),
				tengo.MakeInstruction(parser.OpReturn, 1))),
		fileSet(srcfile{name: "file1", size: 100},
			srcfile{name: "file2", size: 200})))
}

func TestBytecode_RemoveDuplicates(t *testing.T) {
	testBytecodeRemoveDuplicates(t,
		bytecode(
			concatInsts(), objectsArray(
				&tengo.Char{Value: 'y'},
				&tengo.Float{Value: 93.11},
				compiledFunction(1, 0,
					tengo.MakeInstruction(parser.OpConstant, 3),
					tengo.MakeInstruction(parser.OpSetLocal, 0),
					tengo.MakeInstruction(parser.OpGetGlobal, 0),
					tengo.MakeInstruction(parser.OpGetFree, 0)),
				&tengo.Float{Value: 39.2},
				&tengo.Int{Value: 192},
				&tengo.String{Value: "bar"})),
		bytecode(
			concatInsts(), objectsArray(
				&tengo.Char{Value: 'y'},
				&tengo.Float{Value: 93.11},
				compiledFunction(1, 0,
					tengo.MakeInstruction(parser.OpConstant, 3),
					tengo.MakeInstruction(parser.OpSetLocal, 0),
					tengo.MakeInstruction(parser.OpGetGlobal, 0),
					tengo.MakeInstruction(parser.OpGetFree, 0)),
				&tengo.Float{Value: 39.2},
				&tengo.Int{Value: 192},
				&tengo.String{Value: "bar"})))

	testBytecodeRemoveDuplicates(t,
		bytecode(
			concatInsts(
				tengo.MakeInstruction(parser.OpConstant, 0),
				tengo.MakeInstruction(parser.OpConstant, 1),
				tengo.MakeInstruction(parser.OpConstant, 2),
				tengo.MakeInstruction(parser.OpConstant, 3),
				tengo.MakeInstruction(parser.OpConstant, 4),
				tengo.MakeInstruction(parser.OpConstant, 5),
				tengo.MakeInstruction(parser.OpConstant, 6),
				tengo.MakeInstruction(parser.OpConstant, 7),
				tengo.MakeInstruction(parser.OpConstant, 8),
				tengo.MakeInstruction(parser.OpClosure, 4, 1)),
			objectsArray(
				&tengo.Int{Value: 1},
				&tengo.Float{Value: 2.0},
				&tengo.Char{Value: '3'},
				&tengo.String{Value: "four"},
				compiledFunction(1, 0,
					tengo.MakeInstruction(parser.OpConstant, 3),
					tengo.MakeInstruction(parser.OpConstant, 7),
					tengo.MakeInstruction(parser.OpSetLocal, 0),
					tengo.MakeInstruction(parser.OpGetGlobal, 0),
					tengo.MakeInstruction(parser.OpGetFree, 0)),
				&tengo.Int{Value: 1},
				&tengo.Float{Value: 2.0},
				&tengo.Char{Value: '3'},
				&tengo.String{Value: "four"})),
		bytecode(
			concatInsts(
				tengo.MakeInstruction(parser.OpConstant, 0),
				tengo.MakeInstruction(parser.OpConstant, 1),
				tengo.MakeInstruction(parser.OpConstant, 2),
				tengo.MakeInstruction(parser.OpConstant, 3),
				tengo.MakeInstruction(parser.OpConstant, 4),
				tengo.MakeInstruction(parser.OpConstant, 0),
				tengo.MakeInstruction(parser.OpConstant, 1),
				tengo.MakeInstruction(parser.OpConstant, 2),
				tengo.MakeInstruction(parser.OpConstant, 3),
				tengo.MakeInstruction(parser.OpClosure, 4, 1)),
			objectsArray(
				&tengo.Int{Value: 1},
				&tengo.Float{Value: 2.0},
				&tengo.Char{Value: '3'},
				&tengo.String{Value: "four"},
				compiledFunction(1, 0,
					tengo.MakeInstruction(parser.OpConstant, 3),
					tengo.MakeInstruction(parser.OpConstant, 2),
					tengo.MakeInstruction(parser.OpSetLocal, 0),
					tengo.MakeInstruction(parser.OpGetGlobal, 0),
					tengo.MakeInstruction(parser.OpGetFree, 0)))))

	testBytecodeRemoveDuplicates(t,
		bytecode(
			concatInsts(
				tengo.MakeInstruction(parser.OpConstant, 0),
				tengo.MakeInstruction(parser.OpConstant, 1),
				tengo.MakeInstruction(parser.OpConstant, 2),
				tengo.MakeInstruction(parser.OpConstant, 3),
				tengo.MakeInstruction(parser.OpConstant, 4)),
			objectsArray(
				&tengo.Int{Value: 1},
				&tengo.Int{Value: 2},
				&tengo.Int{Value: 3},
				&tengo.Int{Value: 1},
				&tengo.Int{Value: 3})),
		bytecode(
			concatInsts(
				tengo.MakeInstruction(parser.OpConstant, 0),
				tengo.MakeInstruction(parser.OpConstant, 1),
				tengo.MakeInstruction(parser.OpConstant, 2),
				tengo.MakeInstruction(parser.OpConstant, 0),
				tengo.MakeInstruction(parser.OpConstant, 2)),
			objectsArray(
				&tengo.Int{Value: 1},
				&tengo.Int{Value: 2},
				&tengo.Int{Value: 3})))
}

func TestBytecode_CountObjects(t *testing.T) {
	b := bytecode(
		concatInsts(),
		objectsArray(
			&tengo.Int{Value: 55},
			&tengo.Int{Value: 66},
			&tengo.Int{Value: 77},
			&tengo.Int{Value: 88},
			compiledFunction(1, 0,
				tengo.MakeInstruction(parser.OpConstant, 3),
				tengo.MakeInstruction(parser.OpReturn, 1)),
			compiledFunction(1, 0,
				tengo.MakeInstruction(parser.OpConstant, 2),
				tengo.MakeInstruction(parser.OpReturn, 1)),
			compiledFunction(1, 0,
				tengo.MakeInstruction(parser.OpConstant, 1),
				tengo.MakeInstruction(parser.OpReturn, 1))))
	require.Equal(t, 7, b.CountObjects())
}

func fileSet(files ...srcfile) *parser.SourceFileSet {
	fileSet := parser.NewFileSet()
	for _, f := range files {
		fileSet.AddFile(f.name, -1, f.size)
	}
	return fileSet
}

func bytecodeFileSet(
	instructions []byte,
	constants []tengo.Object,
	fileSet *parser.SourceFileSet,
) *tengo.Bytecode {
	return &tengo.Bytecode{
		FileSet:      fileSet,
		MainFunction: &tengo.CompiledFunction{Instructions: instructions},
		Constants:    constants,
	}
}

func testBytecodeRemoveDuplicates(
	t *testing.T,
	input, expected *tengo.Bytecode,
) {
	input.RemoveDuplicates()

	require.Equal(t, expected.FileSet, input.FileSet)
	require.Equal(t, expected.MainFunction, input.MainFunction)
	require.Equal(t, expected.Constants, input.Constants)
}

func testBytecodeSerialization(t *testing.T, b *tengo.Bytecode) {
	var buf bytes.Buffer
	err := b.Encode(&buf)
	require.NoError(t, err)

	r := &tengo.Bytecode{}
	err = r.Decode(bytes.NewReader(buf.Bytes()), nil)
	require.NoError(t, err)

	require.Equal(t, b.FileSet, r.FileSet)
	require.Equal(t, b.MainFunction, r.MainFunction)
	require.Equal(t, b.Constants, r.Constants)
}

func TestBytecodeDecodeTwice(t *testing.T) {
	// decoding into a Bytecode that an earlier Decode already prepared must
	// translate the newly decoded functions too
	first := compileForDecode(t, `f := func() { return 1 }; out = f()`)
	second := compileForDecode(t, `g := func(x) { return x * 2 }; out = g(21)`)
	var b tengo.Bytecode
	decodeRoundTrip(t, first, &b)
	decodeRoundTrip(t, second, &b)

	globals := make([]tengo.Object, tengo.GlobalsSize)
	require.NoError(t, tengo.NewVM(&b, globals, -1).Run())
	require.Equal(t, &tengo.Int{Value: 42}, globals[0])
}

func TestBytecodeDecodeMainStackDepth(t *testing.T) {
	// the main function's stack bound is not serialized; a decoded main that
	// needs more operand slots than the VM has must fail with a stack
	// overflow error instead of running past the end of the stack
	src := "out = [" + strings.Repeat("1, ", tengo.StackSize) + "1]"
	var b tengo.Bytecode
	decodeRoundTrip(t, compileForDecode(t, src), &b)

	globals := make([]tengo.Object, tengo.GlobalsSize)
	err := tengo.NewVM(&b, globals, -1).Run()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "stack overflow"), err.Error())
}

func compileForDecode(t *testing.T, src string) *tengo.Bytecode {
	fileSet := parser.NewFileSet()
	file := fileSet.AddFile("test", -1, len(src))
	parsed, err := parser.NewParser(file, []byte(src), nil).ParseFile()
	require.NoError(t, err)

	symTable := tengo.NewSymbolTable()
	symTable.Define("out")
	c := tengo.NewCompiler(file, symTable, nil, nil, nil)
	require.NoError(t, c.Compile(parsed))
	return c.Bytecode()
}

func decodeRoundTrip(t *testing.T, b *tengo.Bytecode, into *tengo.Bytecode) {
	var buf bytes.Buffer
	require.NoError(t, b.Encode(&buf))
	require.NoError(t, into.Decode(&buf, nil))
}
