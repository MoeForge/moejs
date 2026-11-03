package test262

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/syntax"
)

// fixture is a module that tests import (a _FIXTURE.js file, or a test),
// compiled once per process, and its EarlyExports variant, compiled when a
// graph first needs it.
type fixture struct {
	code, early compiledModule
}

type compiledModule struct {
	once sync.Once
	code *bytecode.Function
	err  error
}

// link links the module test at path, compiled as code from src, with the
// modules it imports.
func (s *Suite) link(testPath string, code *bytecode.Function, src string) (*engine.ModuleGraph, error) {
	return engine.LinkModules(code, s.linkOptions(testPath, code, src))
}

// linkOptions are the options that link the graphs of the test at testPath,
// whose module is code compiled from src (nil for a script test). A
// specifier names a file relative to the directory of the importing module,
// and the test's own file is the test's module. The host value of a module
// is its path, the referrer of the import() calls in its code.
func (s *Suite) linkOptions(testPath string, code *bytecode.Function, src string) engine.LinkOptions {
	return engine.LinkOptions{
		Resolve: func(referrer *bytecode.Function, request int) (*bytecode.Function, error) {
			spec := referrer.Module.Links.Requests[request].Specifier
			return s.resolve(testPath, code, path.Join(path.Dir(referrer.Source.Name), spec))
		},
		EarlyExports: func(c *bytecode.Function) (*bytecode.Function, error) {
			if c == code {
				return compileModule(testPath, src, syntax.Options{EarlyExports: true})
			}
			return s.module(c.Source.Name, true)
		},
		HostDefined: func(c *bytecode.Function) any { return c.Source.Name },
	}
}

// resolve returns the module at p for the test at testPath whose module is
// code: code itself for the test's own file, when the test is a module.
func (s *Suite) resolve(testPath string, code *bytecode.Function, p string) (*bytecode.Function, error) {
	if p == testPath && code != nil {
		return code, nil
	}
	return s.module(p, false)
}

// importHooks returns the import() hooks of the test at testPath, whose
// module is code compiled from src and linked as graph (code and graph are
// nil for a script test, graph also for a module that imports nothing).
// Load resolves a specifier relative to the importing module or script,
// which is the test's file for code without a host value ($262.evalScript),
// and Link links each module once per test: a graph can reach the test's
// own module, which is not shared across tests. A module that does not
// parse, and an import that does not resolve, reject with a SyntaxError.
func (s *Suite) importHooks(testPath string, code *bytecode.Function, src string, graph *engine.ModuleGraph) *engine.ImportHooks {
	graphs := make(map[*bytecode.Function]*engine.ModuleGraph)
	if graph != nil {
		graphs[code] = graph
	}
	return &engine.ImportHooks{
		Load: func(r *engine.Realm, referrer any, specifier string) (any, error) {
			base, ok := referrer.(string)
			if !ok {
				base = testPath
			}
			m, err := s.resolve(testPath, code, path.Join(path.Dir(base), specifier))
			if err != nil {
				return nil, importError(r, err)
			}
			return m, nil
		},
		Link: func(r *engine.Realm, module any) (*engine.ModuleGraph, error) {
			m := module.(*bytecode.Function)
			if g := graphs[m]; g != nil {
				return g, nil
			}
			g, err := engine.LinkModules(m, s.linkOptions(testPath, code, src))
			if err != nil {
				return nil, importError(r, err)
			}
			graphs[m] = g
			return g, nil
		},
	}
}

// importError is what err of loading or linking a module rejects import()
// with: a SyntaxError for a module that does not parse or an import that
// does not resolve, unless moejs does not support its syntax yet, which
// stays an Error so that no test expecting a SyntaxError passes on it.
func importError(r *engine.Realm, err error) error {
	var se *syntax.Error
	var ce *compiler.Error
	var le *engine.LinkError
	if !errors.As(err, &se) && !errors.As(err, &ce) && (!errors.As(err, &le) || le.Err != nil) {
		return err
	}
	msg := compileMessage(err)
	if unsupported(msg) {
		return err
	}
	return r.SyntaxError("%s", strings.TrimPrefix(msg, "SyntaxError: "))
}

// module returns the fixture at p (relative to test/), or its EarlyExports
// variant.
func (s *Suite) module(p string, early bool) (*bytecode.Function, error) {
	v, _ := s.fixtures.LoadOrStore(p, &fixture{})
	c := &v.(*fixture).code
	if early {
		c = &v.(*fixture).early
	}
	c.once.Do(func() {
		b, err := os.ReadFile(filepath.Join(s.Root, "test", filepath.FromSlash(p)))
		if err != nil {
			c.err = err
			return
		}
		c.code, c.err = compileModule(p, string(b), syntax.Options{EarlyExports: early})
	})
	return c.code, c.err
}

func compileModule(name, src string, opts syntax.Options) (*bytecode.Function, error) {
	m, err := syntax.ParseModule(name, src, opts)
	if err != nil {
		return nil, err
	}
	return compiler.CompileModule(m)
}
