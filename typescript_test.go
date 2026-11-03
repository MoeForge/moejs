package moejs_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tsShapes = `
interface Shape { area(): number }
type Kind = "circle" | "square";

abstract class Base<T> implements Shape {
	static count: number = 0;
	declare debug: boolean;
	label?: string;
	id!: number;
	readonly kind: Kind;
	constructor(kind: Kind) { this.kind = kind; Base.count++; }
	abstract area(): number;
	describe(): string;
	describe(prefix: string): string;
	describe(prefix?: string): string { return (prefix ?? "") + this.kind + ":" + this.area().toFixed(1); }
}

class Circle extends Base<number> {
	private r: number;
	constructor(r: number) { super("circle"); this.r = r; }
	area(): number { return Math.PI * this.r ** 2; }
}
`

// TestCompileTS runs a module compiled from TypeScript.
func TestCompileTS(t *testing.T) {
	mod, err := moejs.CompileTS("shapes.ts", tsShapes+`
function pick<T>(xs: T[], i: number): T { return xs[i]!; }
function bound(this: { n: number }, a: number, b?: number): number { return this.n + a + (b ?? 0); }
const square = <T extends number>(x: T): number => x * x;
const config = { retries: 3, name: "x" } as const satisfies Record<string, unknown>;
let shape: Shape = new Circle(1);

export function run(): string {
	const c = new Circle(2) as Base<number>;
	const keys: string[] = Object.keys(new Circle(1));
	return [
		c.describe("> "),
		Base.count,
		keys.join(","),
		"debug" in c,
		pick<string>(["a", "b"], 1),
		bound.length,
		bound.call({ n: 1 }, 2),
		square<number>(3),
		config.retries,
		(shape as Circle).area().toFixed(2),
		Circle.prototype.area.toString(),
	].join("|");
}
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	res, err := rt.Call(mustHook(t, mod, "run"))
	require.NoError(t, err)
	assert.Equal(t, "> circle:12.6|3|label,id,kind,r|false|b|2|3|9|3|3.14|area(): number { return Math.PI * this.r ** 2; }", res.String())
}

// TestCompileTSRejects checks the SyntaxError of TypeScript with run-time
// semantics, at its position in the TypeScript text.
func TestCompileTSRejects(t *testing.T) {
	_, err := moejs.CompileTS("shapes.ts", strings.Replace(tsShapes, "constructor(r: number)", "constructor(private readonly r: number)", 1))
	var se *moejs.SyntaxError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, &moejs.SyntaxError{File: "shapes.ts", Line: 20, Column: 14, Message: "A parameter property is not supported: only erasable TypeScript is"}, se)

	_, err = moejs.CompileTS("e.ts", "let a: number = 1;\nenum E { A, B }")
	require.ErrorAs(t, err, &se)
	assert.Equal(t, "e.ts:2:1: SyntaxError: A TypeScript enum is not supported: only erasable TypeScript is", se.Error())

	_, err = moejs.CompileTS("e.ts", "function f<T>(x: T): T {\n\treturn x +;\n}")
	require.ErrorAs(t, err, &se)
	assert.Equal(t, [2]int{2, 12}, [2]int{se.Line, se.Column})

	_, err = moejs.Compile("e.js", "let a: number = 1;")
	assert.ErrorAs(t, err, &se, "Compile reads JavaScript")
}

// TestCompileTSStack checks that a stack trace refers to the TypeScript
// text.
func TestCompileTSStack(t *testing.T) {
	mod, err := moejs.CompileTS("stack.ts", "type T = { a: number };\nexport function fail(x: T): never {\n\tconst y: string = <string>(x as any);\n\tthrow new Error(y!);\n}")
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	_, err = rt.Call(mustHook(t, mod, "fail"), moejs.String("boom"))
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.True(t, strings.HasPrefix(rt.StackTrace(exc), "Error: boom\n    at fail (stack.ts:4:8)"), rt.StackTrace(exc))
}

// tsHost resolves a specifier to a TypeScript module of srcs, or to a
// virtual module of exports: a module that exports exactly those names,
// so that linking an import of any other name fails.
type tsHost struct {
	t       testing.TB
	srcs    map[string]string
	virtual map[string][]string
	mods    map[string]*moejs.Module
}

func (h *tsHost) resolve(_ moejs.Referrer, specifier string) (*moejs.Module, error) {
	if m := h.mods[specifier]; m != nil {
		return m, nil
	}
	var m *moejs.Module
	var err error
	switch names, virtual := h.virtual[specifier]; {
	case virtual:
		var b strings.Builder
		for _, name := range names {
			fmt.Fprintf(&b, "export const %s = %q;\n", name, specifier+"."+name)
		}
		m, err = moejs.Compile(specifier, b.String())
	case h.srcs[specifier] != "":
		m, err = moejs.CompileTS(specifier, h.srcs[specifier])
	default:
		return nil, errors.New("no module " + specifier)
	}
	if err != nil {
		return nil, err
	}
	if h.mods == nil {
		h.mods = make(map[string]*moejs.Module)
	}
	h.mods[specifier] = m
	return m, nil
}

// TestCompileTSLink links a TypeScript module whose imports name types
// next to values, as a pi extension imports from typebox: the imports used
// only as types are dropped, so the host's virtual modules need not
// export them.
func TestCompileTSLink(t *testing.T) {
	h := &tsHost{t: t, virtual: map[string][]string{
		"typebox": {"Type"},
		"pi":      {"defineTool"},
	}, srcs: map[string]string{
		"./util.ts": `export type Mode = "a" | "b"; export interface Opts { mode: Mode } export const twice = (n: number): number => n * 2; export default class Box<T> { v: T; constructor(v: T) { this.v = v; } }`,
	}}
	entry, err := moejs.CompileTS("ext.ts", `
import { Type, type Static, TSchema } from "typebox";
import type { ExtensionAPI } from "pi";
import * as pi from "pi";
import Box, { twice, type Mode, Opts } from "./util.ts";
import { Unused } from "./util.ts";

const Params = Type;
type P = Static<typeof Params>;
function check(schema: TSchema, opts: Opts, mode: Mode): P { return schema as P; }
export { type Mode, type Opts };
export default function (api: ExtensionAPI): string {
	return [Params, pi.defineTool, twice(21), new Box<number>(1).v].join(" ");
}`)
	require.NoError(t, err)
	assert.Equal(t, []string{"typebox", "pi", "./util.ts"}, entry.Requests())
	linked, err := moejs.Link(entry, h.resolve)
	require.NoError(t, err)
	assert.Equal(t, []string{"default"}, linked.Exports())
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(linked))
	res, err := rt.Call(mustHook(t, linked, "default"))
	require.NoError(t, err)
	assert.Equal(t, "typebox.Type pi.defineTool 42 1", res.String())

	// Without the erasure, the same imports do not link.
	js, err := moejs.Compile("ext.js", `import { Type, TSchema } from "typebox"; export const t = [Type, TSchema];`)
	require.NoError(t, err)
	_, err = moejs.Link(js, h.resolve)
	var se *moejs.SyntaxError
	require.ErrorAs(t, err, &se)
	assert.Contains(t, se.Message, "TSchema")
}

// TestCompileTSCycle links TypeScript modules that import each other, which
// recompiles them with their exports assumed to run early.
func TestCompileTSCycle(t *testing.T) {
	h := &tsHost{t: t, srcs: map[string]string{
		"./a.ts": `import { b } from "./b.ts"; import type { B } from "./b.ts"; export function a(n: number): number { return n <= 0 ? 0 : b(n - 1) + 1; } export const name: string = "a";`,
		"./b.ts": `import { a, name } from "./a.ts"; export interface B {} export function b(n: number): number { return n <= 0 ? name.length : a(n - 1) + 1; }`,
	}}
	entry, err := h.resolve(nil, "./a.ts")
	require.NoError(t, err)
	linked, err := moejs.Link(entry, h.resolve)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(linked))
	res, err := rt.Call(mustHook(t, linked, "a"), moejs.Int(5))
	require.NoError(t, err)
	assert.Equal(t, "6", res.String())
}

// TestCompileTSImporter imports a TypeScript module with import().
func TestCompileTSImporter(t *testing.T) {
	h := &tsHost{t: t, virtual: map[string][]string{"lib": {"value"}}, srcs: map[string]string{
		"./dyn.ts": `import { value, type Value } from "lib"; export const get = (): Value => value as Value;`,
	}}
	mod, err := moejs.CompileTS("main.ts", `export let out: string = "";
export async function load(): Promise<void> {
	const m: typeof import("./dyn.ts") = await import("./dyn.ts");
	out = m.get()!;
}`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{Importer: &moejs.Importer{Resolve: h.resolve}})
	require.NoError(t, rt.Load(mod))
	p, err := rt.Call(mustHook(t, mod, "load"))
	require.NoError(t, err)
	state, _, _ := moejs.PromiseResult(p)
	assert.Equal(t, moejs.PromiseFulfilled, state)
	out, _ := rt.Export("out")
	assert.Equal(t, "lib.value", out.String())
}
