package proc

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
)

// Tracer receives one record per external command run through a Traced
// runner (LEVER_TRACE, lever#184). head is TraceHead(name, args): never the
// whole argv.
type Tracer interface {
	Command(head string, d time.Duration, code int, err error)
}

type tracerKey struct{}

// WithTracer returns ctx carrying t: a Traced runner reports every command
// it runs under ctx to t.
func WithTracer(ctx context.Context, t Tracer) context.Context {
	return context.WithValue(ctx, tracerKey{}, t)
}

func tracerFrom(ctx context.Context) Tracer {
	t, _ := ctx.Value(tracerKey{}).(Tracer)
	return t
}

// Traced wraps r so each command run under a context that carries a Tracer
// is timed and reported. Without a Tracer it adds nothing.
func Traced(r Runner) Runner { return traced{r} }

type traced struct{ r Runner }

func (t traced) Run(ctx context.Context, env map[string]string, name string, args ...string) (Result, error) {
	tr := tracerFrom(ctx)
	if tr == nil {
		return t.r.Run(ctx, env, name, args...)
	}
	start := time.Now()
	res, err := t.r.Run(ctx, env, name, args...)
	tr.Command(TraceHead(name, args), time.Since(start), exitCode(res, err), err)
	return res, err
}

func (t traced) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (Result, error) {
	tr := tracerFrom(ctx)
	if tr == nil {
		return t.r.RunIn(ctx, dir, env, name, args...)
	}
	start := time.Now()
	res, err := t.r.RunIn(ctx, dir, env, name, args...)
	tr.Command(TraceHead(name, args), time.Since(start), exitCode(res, err), err)
	return res, err
}

func (t traced) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (Result, error) {
	tr := tracerFrom(ctx)
	if tr == nil {
		return t.r.RunStdin(ctx, stdin, env, name, args...)
	}
	start := time.Now()
	res, err := t.r.RunStdin(ctx, stdin, env, name, args...)
	tr.Command(TraceHead(name, args), time.Since(start), exitCode(res, err), err)
	return res, err
}

func exitCode(res Result, err error) int {
	if res.Code != 0 {
		return res.Code
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return -1
	}
	return 0
}

// traceWord is an argv word TraceHead may show: a plain command or
// subcommand name, a machine name, a path. Anything else — a flag, a
// KEY=VALUE, a script, a quoted string — ends the head, since past it a
// word can be a secret (`hub secret set NAME VALUE`, `--session-secret=…`,
// `-e KEY=VALUE`, a bash -c script).
var traceWord = regexp.MustCompile(`^[A-Za-z0-9._/:@+-]{1,64}$`)

// traceMaxWords bounds a head.
const traceMaxWords = 5

// secretish names a word that is never shown even when it looks plain.
var secretish = regexp.MustCompile(`(?i)secret|token|passw|credential|key|auth|oauth|sk-ant`)

// TraceHead is the part of a command line safe to log: the program, then
// leading plain words until a flag, an assignment, a non-plain word or a
// secret-looking word, at most traceMaxWords, then the count of the rest.
// It never shows a flag's value, an env value, a script or a positional
// after the first non-plain word.
func TraceHead(name string, args []string) string {
	words := []string{baseName(name)}
	i := 0
	for ; i < len(args) && len(words) < traceMaxWords; i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") || strings.Contains(a, "=") || !traceWord.MatchString(a) {
			break
		}
		if secretish.MatchString(a) {
			words = append(words, "…")
			i = len(args)
			break
		}
		words = append(words, a)
	}
	if rest := len(args) - i; rest > 0 {
		words = append(words, "…(+"+itoa(rest)+")")
	}
	return strings.Join(words, " ")
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	if !traceWord.MatchString(p) {
		return "?"
	}
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
