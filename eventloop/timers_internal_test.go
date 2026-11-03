package eventloop

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDelayConversion checks the delay of a timer: ToNumber of the
// argument truncated to whole milliseconds, and 1 ms for NaN, a value below
// 1 and one above 2147483647, as in Node.
func TestDelayConversion(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	l, err := New(rt, Options{})
	require.NoError(t, err)
	const ms = time.Millisecond
	for _, c := range []struct {
		arg  string
		want time.Duration
	}{
		{"", ms}, {"undefined", ms}, {"null", ms}, {"0", ms}, {"0.5", ms}, {"-1", ms},
		{"NaN", ms}, {"Infinity", ms}, {"2147483648", ms}, {"true", ms},
		{"1", ms}, {"1.5", ms}, {"1.9", ms}, {"2.5", 2 * ms}, {"'10'", 10 * ms}, {"[20]", 20 * ms},
		{"2147483647", 2147483647 * ms},
	} {
		for _, fn := range []string{"setTimeout", "setInterval"} {
			src := fn + "(() => {}"
			if c.arg != "" {
				src += ", " + c.arg
			}
			s, err := moejs.CompileScript("delay.js", src+")")
			require.NoError(t, err)
			v, err := rt.RunScript(s)
			require.NoError(t, err)
			tm := l.timerOf(v)
			require.NotNil(t, tm, fn)
			assert.Equal(t, c.want, time.Duration(tm.delay), "%s(f, %s)", fn, c.arg)
			assert.Equal(t, fn == "setInterval", tm.repeat)
		}
	}
}

// TestStopReturnsItsRun checks that Stop returns the result of the run it
// waited for, also when another run starts before Stop reads it.
func TestStopReturnsItsRun(t *testing.T) {
	l, err := New(moejs.NewRuntime(moejs.Options{}), Options{})
	require.NoError(t, err)
	errFirst := errors.New("first run")
	for range 100 {
		require.NoError(t, l.enter(true))
		got := make(chan error, 1)
		go func() { got <- l.Stop() }()
		for {
			l.mu.Lock()
			stopping := l.stop
			l.mu.Unlock()
			if stopping {
				break
			}
			runtime.Gosched()
		}
		l.exit(errFirst)
		require.NoError(t, l.enter(false), "another run starts before Stop reads")
		assert.Equal(t, errFirst, <-got)
		l.exit(nil)
	}
}
