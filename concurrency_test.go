package tengo_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d5/tengo/v2"
	"github.com/d5/tengo/v2/require"
	"github.com/d5/tengo/v2/stdlib"
)

// These tests exercise the shared-script pattern: compile a script once,
// keep the Compiled, and for every execution Clone it, swap builtin modules,
// run it under a context and read its globals. Run them with -race.

// counterModule keeps state at module level and hands out closures over it;
// a run must see a fresh instance and never another run's state.
const counterModule = `
state := {n: 0}
export {
	inc: func() { state.n += 1; return state.n },
	get: func() { return state.n }
}
`

// sharedScript is the script compiled once. It imports a source module, a
// stdlib module and the builtin module "host" that each execution replaces.
const sharedScript = `
m := import("counter")
text := import("text")
host := import("host")

for i := 0; i < 50; i++ { m.inc() }
keys := []
for i := 0; i < 20; i++ { keys = append(keys, text.join(["k", string(i)], "")) }
tbl := {}
for k in keys { tbl[k] = len(k) + host.id() }
sum := 0
for k, v in tbl { sum += tbl[k] }
host.emit(sum)
out = m.get() * 1000 + host.id()
`

type hostModule struct {
	id   int
	seen []int64
}

func (s *hostModule) module() map[string]tengo.Object {
	return map[string]tengo.Object{
		"id": &tengo.UserFunction{Name: "id", Value: func(args ...tengo.Object) (tengo.Object, error) {
			return &tengo.Int{Value: int64(s.id)}, nil
		}},
		"emit": &tengo.UserFunction{Name: "emit", Value: func(args ...tengo.Object) (tengo.Object, error) {
			v, _ := tengo.ToInt64(args[0])
			s.seen = append(s.seen, v)
			return tengo.UndefinedValue, nil
		}},
	}
}

func compileShared(t *testing.T, src string) *tengo.Compiled {
	mods := stdlib.GetModuleMap("text", "fmt", "json").Copy()
	mods.AddSourceModule("counter", []byte(counterModule))
	mods.AddBuiltinModule("host", (&hostModule{}).module())

	scr := tengo.NewScript([]byte(src))
	scr.SetImports(mods)
	require.NoError(t, scr.Add("out", 0))
	compiled, err := scr.Compile()
	require.NoError(t, err)
	return compiled
}

// runClone does what one execution does with the shared Compiled.
func runClone(ctx context.Context, shared *tengo.Compiled, sink *hostModule) (*tengo.Compiled, error) {
	c := shared.Clone()
	c.ReplaceBuiltinModule("host", sink.module())
	c.ReplaceBuiltinModule("__missing__", map[string]tengo.Object{}) // callers often swap several
	return c, c.RunContext(ctx)
}

func expectedSum(id int) int64 {
	var sum int64
	for i := 0; i < 20; i++ {
		sum += int64(len(fmt.Sprintf("k%d", i)) + id)
	}
	return sum
}

func TestConcurrentClonesOfSharedScript(t *testing.T) {
	shared := compileShared(t, sharedScript)

	const workers, rounds = 32, 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				id := w*rounds + r + 1
				sink := &hostModule{id: id}
				c, err := runClone(context.Background(), shared, sink)
				require.NoError(t, err)
				// module state is per run: 50 increments regardless of what
				// other executions did
				require.Equal(t, int64(50*1000+id), c.Get("out").Int64())
				require.Equal(t, 1, len(sink.seen))
				require.Equal(t, expectedSum(id), sink.seen[0])
			}
		}(w)
	}
	// the owner sizes and clones the original while clones are running
	for i := 0; i < 50; i++ {
		_ = shared.Size()
		_ = shared.Clone()
	}
	wg.Wait()

	// the original was never run and still has its initial globals
	require.Equal(t, int64(0), shared.Get("out").Int64())
}

func TestConcurrentClonesSeeFreshModuleState(t *testing.T) {
	// the same source module is imported by every clone; a pooled VM must
	// not carry one run's memoized module exports into the next run
	shared := compileShared(t, `m := import("counter"); out = m.inc()`)

	run := func(c *tengo.Compiled) {
		require.NoError(t, c.Run())
		require.Equal(t, int64(1), c.Get("out").Int64())
	}
	// sequential runs reuse pooled VMs
	for i := 0; i < 20; i++ {
		run(shared.Clone())
	}
	// a clone run twice gets a fresh module instance both times
	c := shared.Clone()
	run(c)
	run(c)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(shared.Clone())
		}()
	}
	wg.Wait()
}

func TestConcurrentClonesWithTimeout(t *testing.T) {
	shared := compileShared(t, sharedScript)
	spinner := compileShared(t, `for { out += 1 }`)

	var wg sync.WaitGroup
	var timeouts int64
	for w := 0; w < 16; w++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
			defer cancel()
			c := spinner.Clone()
			err := c.RunContext(ctx)
			require.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
			atomic.AddInt64(&timeouts, 1)
			// the aborted clone can run again, to completion, with a fresh deadline
			ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Millisecond)
			defer cancel2()
			require.True(t, errors.Is(c.RunContext(ctx2), context.DeadlineExceeded))
		}(w)
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sink := &hostModule{id: id}
			c, err := runClone(ctx, shared, sink)
			require.NoError(t, err)
			require.Equal(t, int64(50*1000+id), c.Get("out").Int64())
		}(w + 1)
	}
	wg.Wait()
	require.Equal(t, int64(16), atomic.LoadInt64(&timeouts))

	// VMs that were aborted go back to the pool; later runs must not
	// inherit the abort
	for i := 0; i < 32; i++ {
		sink := &hostModule{id: 7}
		c, err := runClone(context.Background(), shared, sink)
		require.NoError(t, err)
		require.Equal(t, int64(50*1000+7), c.Get("out").Int64())
	}
}

var errExitScript = errors.New("script exit")

func TestConcurrentClonesUserFunctionErrorsAndPanics(t *testing.T) {
	shared := compileShared(t, `
host := import("host")
m := import("counter")
m.inc()
host.fail()
out = 99
`)
	failing := func(mode int) map[string]tengo.Object {
		return map[string]tengo.Object{
			"fail": &tengo.UserFunction{Name: "fail", Value: func(args ...tengo.Object) (tengo.Object, error) {
				switch mode {
				case 1:
					return nil, errExitScript // a host function stopping the script early
				case 2:
					panic("user function panicked")
				}
				return tengo.UndefinedValue, nil
			}},
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < 48; w++ {
		wg.Add(1)
		go func(mode int) {
			defer wg.Done()
			c := shared.Clone()
			c.ReplaceBuiltinModule("host", failing(mode))
			err := c.RunContext(context.Background())
			switch mode {
			case 0:
				require.NoError(t, err)
				require.Equal(t, int64(99), c.Get("out").Int64())
			case 1:
				require.True(t, errors.Is(err, errExitScript), "got %v", err)
				require.Equal(t, int64(0), c.Get("out").Int64())
			case 2:
				require.Error(t, err)
				require.Equal(t, int64(0), c.Get("out").Int64())
			}
		}(w % 3)
	}
	wg.Wait()

	// pooled VMs that saw an error or a recovered panic run cleanly again
	for i := 0; i < 32; i++ {
		c := shared.Clone()
		c.ReplaceBuiltinModule("host", failing(0))
		require.NoError(t, c.Run())
		require.Equal(t, int64(99), c.Get("out").Int64())
	}
}

func TestRunOriginalWhileCloning(t *testing.T) {
	// the owner runs the original itself while other goroutines clone it,
	// and keeps swapping its modules between runs
	shared := compileShared(t, sharedScript)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			sink := &hostModule{id: 1}
			shared.ReplaceBuiltinModule("host", sink.module())
			require.NoError(t, shared.RunContext(context.Background()))
			require.Equal(t, int64(50*1000+1), shared.Get("out").Int64())
		}
	}()
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 4; i++ {
				sink := &hostModule{id: id}
				c, err := runClone(context.Background(), shared, sink)
				require.NoError(t, err)
				require.Equal(t, int64(50*1000+id), c.Get("out").Int64())
			}
		}(w + 10)
	}
	wg.Wait()
}

func TestConcurrentCompileOfDifferentScripts(t *testing.T) {
	// distinct scripts compile in parallel; they share the stdlib module
	// map, the builtin table and the compiler's shared state
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			src := fmt.Sprintf(`
m := import("counter")
host := import("host")
f := func(x) { return x * %d }
for i := 0; i < %d; i++ { m.inc() }
out = f(m.get()) + host.id()
`, id+1, id+1)
			c := compileShared(t, src)
			sink := &hostModule{id: id}
			c, err := runClone(context.Background(), c, sink)
			require.NoError(t, err)
			require.Equal(t, int64((id+1)*(id+1)+id), c.Get("out").Int64())
		}(w)
	}
	wg.Wait()
}
