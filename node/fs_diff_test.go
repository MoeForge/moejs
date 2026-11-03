package node_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// The differential test runs the cases of testdata/fsprobe.mjs in moejs
// and compares their results with Node.js 24's, in testdata/fs_node.json
// (testdata/fsgen.mjs writes it), over trees built from
// testdata/fstree.json. The fields that depend on the tree's instance
// (device, inode, times, owner, blocks) or on its file system (a
// directory's nlink and size) are compared only with MOEJS_NODE set to a
// node binary: the test then runs the read cases in Node.js over the very
// tree moejs read too.

type treeSpec struct {
	Dirs  []string          `json:"dirs"`
	Files map[string]string `json:"files"`
	Links map[string]string `json:"links"`
	Modes map[string]int    `json:"modes"`
}

var hexEscape = regexp.MustCompile(`\\x([0-9a-f]{2})`)

func makeSpecTree(t *testing.T, dir string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/fstree.json")
	require.NoError(t, err)
	var spec treeSpec
	require.NoError(t, json.Unmarshal(raw, &spec))
	for _, d := range spec.Dirs {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, d), 0o775))
	}
	for name, content := range spec.Files {
		data := []byte(content)
		if strings.Contains(content, `\x`) {
			data = []byte(hexEscape.ReplaceAllStringFunc(content, func(m string) string {
				n, _ := strconv.ParseUint(m[2:], 16, 8)
				return string([]byte{byte(n)})
			}))
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o664))
	}
	for name, target := range spec.Links {
		require.NoError(t, os.Symlink(target, filepath.Join(dir, name)))
	}
	for name, mode := range spec.Modes {
		require.NoError(t, os.Chmod(filepath.Join(dir, name), os.FileMode(mode)))
	}
}

// runProbe runs probeRead or probeWrite of fsprobe.mjs in moejs over dir,
// through a module that imports node:fs statically.
func runProbe(t *testing.T, which, dir string, write bool) map[string]any {
	t.Helper()
	src, err := os.ReadFile("testdata/fsprobe.mjs")
	require.NoError(t, err)
	probe, err := moejs.Compile("fsprobe.mjs", string(src))
	require.NoError(t, err)
	main, err := moejs.Compile("main.mjs", `import fs from "node:fs";
import { Buffer } from "node:buffer";
import { probeRead, probeWrite } from "./fsprobe.mjs";
export async function main(which, root) {
	const probe = which === "read" ? probeRead : probeWrite;
	return JSON.stringify(await probe(fs, fs.promises, Buffer, root));
}`)
	require.NoError(t, err)
	resolve := node.Resolver(func(_ moejs.Referrer, spec string) (*moejs.Module, error) {
		if spec == "./fsprobe.mjs" {
			return probe, nil
		}
		return nil, os.ErrNotExist
	})
	linked, err := moejs.Link(main, resolve)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	api, err := node.InstallLoop(loop, node.Options{FS: &node.FSOptions{Root: dir, Write: write}})
	require.NoError(t, err)
	defer api.Close()
	hook, err := linked.Hook("main")
	require.NoError(t, err)
	var p moejs.Value
	require.NoError(t, loop.Run(func(rt *moejs.Runtime) error {
		if err := rt.Load(linked); err != nil {
			return err
		}
		var err error
		p, err = rt.Call(hook, moejs.String(which), moejs.String(dir))
		return err
	}))
	state, res, ok := moejs.PromiseResult(p)
	require.True(t, ok)
	require.Equal(t, moejs.PromiseFulfilled, state, "the probe failed: %s", res.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.String()), &out))
	return out
}

// volatile are the Stats fields that differ between two trees built
// alike. birthtimeMs differs from Node's statx birth time on Linux, where
// moejs gives ctimeMs, as libuv does without statx.
var volatile = map[string]bool{
	"dev": true, "ino": true, "atimeMs": true, "mtimeMs": true, "ctimeMs": true, "birthtimeMs": true,
	"mtime": true, "uid": true, "gid": true, "blocks": true, "blksize": true,
}

// volatileDir are the Stats fields of a directory that differ between
// file systems: tmpfs and ext4 count a directory's subdirectories in its
// nlink, btrfs and APFS give 1, and its size is 0 on btrfs and APFS, 4096
// on ext4 and grows with its entries on tmpfs.
var volatileDir = map[string]bool{"nlink": true, "size": true}

// mask drops the volatile fields of the Stats in v, all of them, or with
// live only those two reads of one tree may see differ.
func mask(v any, live bool) any {
	switch x := v.(type) {
	case map[string]any:
		_, isStats := x["isFile"]
		isDir := x["isDirectory"] == true
		out := map[string]any{}
		for k, e := range x {
			if isStats && volatile[k] && (!live || k == "atimeMs" || k == "birthtimeMs") {
				continue
			}
			if isStats && isDir && volatileDir[k] && !live {
				continue
			}
			out[k] = mask(e, live)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = mask(e, live)
		}
		return out
	}
	return v
}

// compareCases fails for each case whose result differs.
func compareCases(t *testing.T, want, got map[string]any, live bool) {
	t.Helper()
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s: missing in moejs", name)
			continue
		}
		wj, _ := json.Marshal(mask(w, live))
		gj, _ := json.Marshal(mask(g, live))
		if string(wj) != string(gj) {
			t.Errorf("%s:\n  node:  %s\n  moejs: %s", name, wj, gj)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: missing in node", name)
		}
	}
}

func goldenCases(t *testing.T) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/fs_node.json")
	require.NoError(t, err)
	var golden map[string]map[string]any
	require.NoError(t, json.Unmarshal(raw, &golden))
	return golden
}

// realDir returns a new temporary directory with its links resolved, as
// Node's realpath gives it.
func realDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func TestNodeDiffRead(t *testing.T) {
	dir := realDir(t)
	makeSpecTree(t, dir)
	// With Write, so that access(W_OK) checks the mode as Node.js does.
	got := runProbe(t, "read", dir, true)
	compareCases(t, goldenCases(t)["read"], got, false)
	if bin := os.Getenv("MOEJS_NODE"); bin != "" {
		cmd := exec.Command(bin, "--no-warnings", "testdata/fsgen.mjs", "read", dir)
		raw, err := cmd.Output()
		require.NoError(t, err)
		var live map[string]any
		require.NoError(t, json.Unmarshal(raw, &live))
		compareCases(t, live, got, true)
	}
}

func TestNodeDiffWrite(t *testing.T) {
	dir := realDir(t)
	makeSpecTree(t, dir)
	got := runProbe(t, "write", dir, true)
	compareCases(t, goldenCases(t)["write"], got, false)
}
