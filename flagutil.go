package flagutil

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/gobwas/flagutil/parse"
)

var SetSeparator = "."

type Parser interface {
	Parse(context.Context, parse.FlagSet) error
}

type ParserFunc func(context.Context, parse.FlagSet) error

func (fn ParserFunc) Parse(ctx context.Context, fs parse.FlagSet) error {
	return fn(ctx, fs)
}

type Printer interface {
	Name(context.Context, parse.FlagSet) (func(*flag.Flag, func(string)), error)
}

type PrinterFunc func(context.Context, parse.FlagSet) (func(*flag.Flag, func(string)), error)

func (fn PrinterFunc) Name(ctx context.Context, fs parse.FlagSet) (func(*flag.Flag, func(string)), error) {
	return fn(ctx, fs)
}

type parser struct {
	Parser
	stash func(*flag.Flag) bool
}

type config struct {
	parsers          []*parser
	parserOptions    []ParserOption
	ignoreUndefined  bool
	customUsage      bool
	unquoteUsageMode UnquoteUsageMode
}

func buildConfig(opts []ParseOption) config {
	c := config{
		unquoteUsageMode: UnquoteDefault,
	}
	for _, opt := range opts {
		opt.setupParseConfig(&c)
	}
	for _, opt := range c.parserOptions {
		for _, p := range c.parsers {
			opt.setupParserConfig(p)
		}
	}
	return c
}

func Parse(ctx context.Context, flags *flag.FlagSet, opts ...ParseOption) (err error) {
	c := buildConfig(opts)

	fs := parse.NewFlagSet(flags,
		parse.WithIgnoreUndefined(c.ignoreUndefined),
	)
	for _, p := range c.parsers {
		parse.NextLevel(fs)
		parse.Stash(fs, p.stash)

		if err = p.Parse(ctx, fs); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				_ = printUsageMaybe(ctx, &c, flags)
			}
			if err != nil {
				err = fmt.Errorf("flagutil: parse error: %w", err)
			}
			switch flags.ErrorHandling() {
			case flag.ContinueOnError:
				return err
			case flag.ExitOnError:
				if !errors.Is(err, flag.ErrHelp) {
					fmt.Fprintf(flags.Output(), "%v\n", err)
				}
				os.Exit(2)
			case flag.PanicOnError:
				panic(err.Error())
			}
		}
	}
	return nil
}

// PrintDefaults prints parsers aware usage message to flags.Output().
func PrintDefaults(ctx context.Context, flags *flag.FlagSet, opts ...ParseOption) error {
	c := buildConfig(opts)
	return printDefaults(ctx, &c, flags)
}

func printUsageMaybe(ctx context.Context, c *config, flags *flag.FlagSet) error {
	if !c.customUsage && flags.Usage != nil {
		flags.Usage()
		return nil
	}
	if name := flags.Name(); name == "" {
		fmt.Fprintf(flags.Output(), "Usage:\n")
	} else {
		fmt.Fprintf(flags.Output(), "Usage of %s:\n", name)
	}
	return printDefaults(ctx, c, flags)
}

type UnquoteUsageMode uint8

const (
	UnquoteNothing UnquoteUsageMode = 1 << iota >> 1
	UnquoteQuoted
	UnquoteInferType
	UnquoteClean

	UnquoteDefault UnquoteUsageMode = UnquoteQuoted | UnquoteInferType
)

func (m UnquoteUsageMode) String() string {
	switch m {
	case UnquoteNothing:
		return "UnquoteNothing"
	case UnquoteQuoted:
		return "UnquoteQuoted"
	case UnquoteInferType:
		return "UnquoteInferType"
	case UnquoteClean:
		return "UnquoteClean"
	case UnquoteDefault:
		return "UnquoteDefault"
	default:
		return "<unknown>"
	}
}

func (m UnquoteUsageMode) has(x UnquoteUsageMode) bool {
	return m&x != 0
}

func printDefaults(ctx context.Context, c *config, flags *flag.FlagSet) (err error) {
	fs := parse.NewFlagSet(flags)

	var hasNameFunc bool
	nameFunc := make([]func(*flag.Flag, func(string)), len(c.parsers))
	for i := len(c.parsers) - 1; i >= 0; i-- {
		if p, ok := c.parsers[i].Parser.(Printer); ok {
			hasNameFunc = true
			nameFunc[i], err = p.Name(ctx, fs)
			if err != nil {
				return
			}
		}
	}

	var buf bytes.Buffer
	flags.VisitAll(func(f *flag.Flag) {
		n, _ := buf.WriteString("  ")
		for i := len(c.parsers) - 1; i >= 0; i-- {
			fn := nameFunc[i]
			if fn == nil {
				continue
			}
			if stash := c.parsers[i].stash; stash != nil && stash(f) {
				continue
			}
			fn(f, func(name string) {
				if buf.Len() > n {
					buf.WriteString(", ")
				}
				buf.WriteString(name)
			})
		}
		if buf.Len() == n {
			// No name has been given.
			// Two cases are possible: no Printer implementation among parsers;
			// or some parser intentionally filtered out this flag.
			if hasNameFunc {
				buf.Reset()
				return
			}
			buf.WriteString(f.Name)
		}
		name, usage := unquoteUsage(c.unquoteUsageMode, f)
		if len(name) > 0 {
			buf.WriteString("\n    \t")
			buf.WriteString(name)
		}
		buf.WriteString("\n    \t")
		if len(usage) > 0 {
			buf.WriteString(strings.ReplaceAll(usage, "\n", "\n    \t"))
			buf.WriteString(" (")
		}
		buf.WriteString("default ")
		buf.WriteString(defValue(f))
		if len(usage) > 0 {
			buf.WriteString(")")
		}

		buf.WriteByte('\n')
		buf.WriteByte('\n')
		buf.WriteTo(flags.Output())
	})

	return nil
}

func defValue(f *flag.Flag) string {
	v := reflect.ValueOf(f.Value)
repeat:
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
		goto repeat
	}
	d := f.DefValue
	if d == "" {
		switch v.Kind() {
		case reflect.String:
			return `""`
		case
			reflect.Slice,
			reflect.Array:
			return `[]`
		case
			reflect.Struct,
			reflect.Map:
			return `{}`
		}
		return "?"
	}
	if v.Kind() == reflect.String {
		return `"` + d + `"`
	}
	return d
}

// unquoteUsage is the same as flag.UnquoteUsage() with exception that it does
// not infer type of the flag value.
func unquoteUsage(m UnquoteUsageMode, f *flag.Flag) (name, usage string) {
	if m == UnquoteNothing {
		return "", f.Usage
	}
	u := f.Usage
	i := strings.IndexByte(u, '`')
	if i == -1 {
		if m.has(UnquoteInferType) {
			return inferType(f), f.Usage
		}
		return "", u
	}
	j := strings.IndexByte(u[i+1:], '`')
	if j == -1 {
		if m.has(UnquoteInferType) {
			return inferType(f), f.Usage
		}
		return "", u
	}
	j += i + 1

	switch {
	case m.has(UnquoteQuoted):
		name = u[i+1 : j]
	case m.has(UnquoteInferType):
		name = inferType(f)
	}

	prefix := u[:i]
	suffix := u[j+1:]
	switch {
	case m.has(UnquoteClean):
		usage = "" +
			strings.TrimRight(prefix, " ") +
			" " +
			strings.TrimLeft(suffix, " ")

	case m.has(UnquoteQuoted):
		usage = prefix + name + suffix

	default:
		usage = f.Usage
	}

	return
}

func inferType(f *flag.Flag) string {
	if f.Value == nil {
		return "?"
	}
	if isBoolFlag(f) {
		return "bool"
	}

	var x interface{}
	if g, ok := f.Value.(flag.Getter); ok {
		x = g.Get()
	} else {
		x = f.Value
	}
	v := reflect.ValueOf(x)

repeat:
	switch v.Type() {
	case reflect.TypeOf(time.Duration(0)):
		return "duration"
	}
	switch v.Kind() {
	case
		reflect.Interface,
		reflect.Ptr:

		v = v.Elem()
		goto repeat

	case
		reflect.String:
		return "string"
	case
		reflect.Float32,
		reflect.Float64:
		return "float"
	case
		reflect.Int,
		reflect.Int8,
		reflect.Int16,
		reflect.Int32,
		reflect.Int64:
		return "int"
	case
		reflect.Uint,
		reflect.Uint8,
		reflect.Uint16,
		reflect.Uint32,
		reflect.Uint64:
		return "uint"
	case
		reflect.Slice,
		reflect.Array:
		return "list"
	case
		reflect.Map:
		return "object"
	}
	return ""
}

// Subset registers new flag subset with given prefix within given flag
// superset. It calls setup function to let caller register needed flags within
// created subset.
func Subset(super *flag.FlagSet, prefix string, setup func(sub *flag.FlagSet)) (err error) {
	sub := flag.NewFlagSet(prefix, 0)
	setup(sub)
	sub.VisitAll(func(f *flag.Flag) {
		name := prefix + "." + f.Name
		if super.Lookup(name) != nil && err == nil {
			err = fmt.Errorf(
				"flag %q already exists in a super set",
				name,
			)
			return
		}
		super.Var(f.Value, name, f.Usage)
	})
	return
}

func isBoolFlag(f *flag.Flag) bool {
	x, ok := f.Value.(interface {
		IsBoolFlag() bool
	})
	return ok && x.IsBoolFlag()
}
