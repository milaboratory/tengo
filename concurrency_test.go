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

// These tests mirror how an embedding backend uses a cached compiled
// template: compile once, then per render Clone, swap builtin modules,
// run under a context, and read globals. Run them with -race.

// counterModule keeps state at module level and hands out closures over it;
// a run must see a fresh instance and never another run's state.
const counterModule = `
state := {n: 0}
export {
	inc: func() { state.n += 1; return state.n },
	get: func() { return state.n }
}
`

// sharedTemplate is the cached script. It imports a source module, a stdlib
// module and the builtin module "rt" that each render replaces.
const sharedTemplate = `
m := import("counter")
text := import("text")
rt := import("rt")

for i := 0; i < 50; i++ { m.inc() }
keys := []
for i := 0; i < 20; i++ { keys = append(keys, text.join(["k", string(i)], "")) }
tbl := {}
for k in keys { tbl[k] = len(k) + rt.id() }
sum := 0
for k, v in tbl { sum += tbl[k] }
rt.emit(sum)
out = m.get() * 1000 + rt.id()
`

type runtimeSink struct {
	id   int
	seen []int64
}

func (s *runtimeSink) module() map[string]tengo.Object {
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

func compileSharedTemplate(t *testing.T, src string) *tengo.Compiled {
	mods := stdlib.GetModuleMap("text", "fmt", "json").Copy()
	mods.AddSourceModule("counter", []byte(counterModule))
	mods.AddBuiltinModule("rt", (&runtimeSink{}).module())

	scr := tengo.NewScript([]byte(src))
	scr.SetImports(mods)
	require.NoError(t, scr.Add("out", 0))
	compiled, err := scr.Compile()
	require.NoError(t, err)
	return compiled
}

// renderClone does what one render does with the cached template.
func renderClone(ctx context.Context, cached *tengo.Compiled, sink *runtimeSink) (*tengo.Compiled, error) {
	c := cached.Clone()
	c.ReplaceBuiltinModule("rt", sink.module())
	c.ReplaceBuiltinModule("__missing__", map[string]tengo.Object{}) // the backend swaps several
	return c, c.RunContext(ctx)
}

func expectedSum(id int) int64 {
	var sum int64
	for i := 0; i < 20; i++ {
		sum += int64(len(fmt.Sprintf("k%d", i)) + id)
	}
	return sum
}

func TestConcurrentRendersOfCachedTemplate(t *testing.T) {
	cached := compileSharedTemplate(t, sharedTemplate)

	const workers, rounds = 32, 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				id := w*rounds + r + 1
				sink := &runtimeSink{id: id}
				c, err := renderClone(context.Background(), cached, sink)
				require.NoError(t, err)
				// module state is per run: 50 increments regardless of what
				// other renders did
				require.Equal(t, int64(50*1000+id), c.Get("out").Int64())
				require.Equal(t, 1, len(sink.seen))
				require.Equal(t, expectedSum(id), sink.seen[0])
			}
		}(w)
	}
	// the cache weighs and clones the original while renders are running
	for i := 0; i < 50; i++ {
		_ = cached.Size()
		_ = cached.Clone()
	}
	wg.Wait()

	// the original was never run and still has its initial globals
	require.Equal(t, int64(0), cached.Get("out").Int64())
}

func TestConcurrentRendersSeeFreshModuleState(t *testing.T) {
	// the same source module is imported by every render; a pooled VM must
	// not carry one run's memoized module exports into the next run
	cached := compileSharedTemplate(t, `m := import("counter"); out = m.inc()`)

	run := func(c *tengo.Compiled) {
		require.NoError(t, c.Run())
		require.Equal(t, int64(1), c.Get("out").Int64())
	}
	// sequential runs reuse pooled VMs
	for i := 0; i < 20; i++ {
		run(cached.Clone())
	}
	// a clone run twice gets a fresh module instance both times
	c := cached.Clone()
	run(c)
	run(c)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(cached.Clone())
		}()
	}
	wg.Wait()
}

func TestConcurrentRendersWithTimeout(t *testing.T) {
	cached := compileSharedTemplate(t, sharedTemplate)
	spinner := compileSharedTemplate(t, `for { out += 1 }`)

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
			sink := &runtimeSink{id: id}
			c, err := renderClone(ctx, cached, sink)
			require.NoError(t, err)
			require.Equal(t, int64(50*1000+id), c.Get("out").Int64())
		}(w + 1)
	}
	wg.Wait()
	require.Equal(t, int64(16), atomic.LoadInt64(&timeouts))

	// VMs that were aborted go back to the pool; later renders must not
	// inherit the abort
	for i := 0; i < 32; i++ {
		sink := &runtimeSink{id: 7}
		c, err := renderClone(context.Background(), cached, sink)
		require.NoError(t, err)
		require.Equal(t, int64(50*1000+7), c.Get("out").Int64())
	}
}

var errExitScript = errors.New("script exit")

func TestConcurrentRendersUserFunctionErrorsAndPanics(t *testing.T) {
	cached := compileSharedTemplate(t, `
rt := import("rt")
m := import("counter")
m.inc()
rt.fail()
out = 99
`)
	failing := func(mode int) map[string]tengo.Object {
		return map[string]tengo.Object{
			"fail": &tengo.UserFunction{Name: "fail", Value: func(args ...tengo.Object) (tengo.Object, error) {
				switch mode {
				case 1:
					return nil, errExitScript // the backend's .exit() path
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
			c := cached.Clone()
			c.ReplaceBuiltinModule("rt", failing(mode))
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
		c := cached.Clone()
		c.ReplaceBuiltinModule("rt", failing(0))
		require.NoError(t, c.Run())
		require.Equal(t, int64(99), c.Get("out").Int64())
	}
}

func TestRunOriginalWhileCloning(t *testing.T) {
	// on a cache fault the backend runs the freshly compiled script itself
	// and caches it; later renders clone it while it may still be running,
	// and the owner may keep swapping its modules between runs
	cached := compileSharedTemplate(t, sharedTemplate)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			sink := &runtimeSink{id: 1}
			cached.ReplaceBuiltinModule("rt", sink.module())
			require.NoError(t, cached.RunContext(context.Background()))
			require.Equal(t, int64(50*1000+1), cached.Get("out").Int64())
		}
	}()
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 4; i++ {
				sink := &runtimeSink{id: id}
				c, err := renderClone(context.Background(), cached, sink)
				require.NoError(t, err)
				require.Equal(t, int64(50*1000+id), c.Get("out").Int64())
			}
		}(w + 10)
	}
	wg.Wait()
}

func TestConcurrentCompileOfDifferentTemplates(t *testing.T) {
	// distinct templates compile in parallel on cache misses; they share the
	// stdlib module map, the builtin table and the compiler's shared state
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			src := fmt.Sprintf(`
m := import("counter")
rt := import("rt")
f := func(x) { return x * %d }
for i := 0; i < %d; i++ { m.inc() }
out = f(m.get()) + rt.id()
`, id+1, id+1)
			c := compileSharedTemplate(t, src)
			sink := &runtimeSink{id: id}
			c, err := renderClone(context.Background(), c, sink)
			require.NoError(t, err)
			require.Equal(t, int64((id+1)*(id+1)+id), c.Get("out").Int64())
		}(w)
	}
	wg.Wait()
}
