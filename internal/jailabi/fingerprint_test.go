package jailabi

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite fingerprint.txt for the current in-jail sources (bump JailABI first)")

const modulePath = "github.com/stevegeek/lever"

// jailRoots are the programs baked into the agent image (Makefile
// lever-image / lever-image-bins) plus the packages that define what they
// speak. Everything they import from this module joins them through
// `go list -deps`.
var jailRoots = []string{
	"./cmd/lever-agent",
	"./cmd/lever-manager",
	"./cmd/lever-tool-db",
	"./internal/agent",
	"./internal/wire",
}

// jailExtraFiles are non-Go files that shape the image: the pre-start hook
// it copies in, and the Dockerfile (its ENV lines are a host-jail contract).
var jailExtraFiles = []string{
	"cmd/lever-agent/scionhook/pre-start",
	"image/lever-claude/Dockerfile",
}

// jailArches are the jail architectures the image is built for; the package
// set is the union over them.
var jailArches = []string{"amd64", "arm64"}

const updateCmd = "go test ./internal/jailabi -run TestJailFingerprint -update"

type listedPkg struct {
	ImportPath     string
	Dir            string
	Module         *struct{ Path, Version string }
	GoFiles        []string
	CgoFiles       []string
	IgnoredGoFiles []string
	EmbedFiles     []string
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	return root
}

func goTool() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// jailSources lists the in-jail packages of this module (import paths,
// sorted), the external modules they link (path@version, sorted), and the
// files to hash (repo-relative, slash-separated, sorted). The package set is
// the union over linux and each jail arch; every Go file of a package counts,
// whatever its build constraints, so the result does not depend on the host
// that runs the test.
func jailSources(t *testing.T, root string) (pkgs, mods, files []string) {
	t.Helper()
	for _, arch := range jailArches {
		p, m, f := listJail(t, root, arch)
		pkgs, mods, files = append(pkgs, p...), append(mods, m...), append(files, f...)
	}
	files = append(files, jailExtraFiles...)
	for _, s := range []*[]string{&pkgs, &mods, &files} {
		slices.Sort(*s)
		*s = slices.Compact(*s)
	}
	return pkgs, mods, files
}

func listJail(t *testing.T, root, arch string) (pkgs, mods, files []string) {
	t.Helper()
	cmd := exec.Command(goTool(), append([]string{"list", "-deps", "-json"}, jailRoots...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("go list output: %v", err)
		}
		if p.Module == nil {
			continue // the standard library: covered by the go line of go.mod
		}
		if p.Module.Path != modulePath {
			mods = append(mods, p.Module.Path+"@"+p.Module.Version)
			continue
		}
		pkgs = append(pkgs, p.ImportPath)
		rel, err := filepath.Rel(root, p.Dir)
		if err != nil {
			t.Fatal(err)
		}
		// go test caches a result by the files and directories the test
		// itself opened; go list runs in a child it cannot see. Reading the
		// directory makes a file added to the package invalidate the cache.
		if _, err := os.ReadDir(p.Dir); err != nil {
			t.Fatal(err)
		}
		for _, group := range [][]string{p.GoFiles, p.CgoFiles, p.IgnoredGoFiles, p.EmbedFiles} {
			for _, f := range group {
				if strings.HasSuffix(f, "_test.go") {
					continue
				}
				files = append(files, filepath.ToSlash(filepath.Join(rel, f)))
			}
		}
	}
	return pkgs, mods, files
}

// goModPins is the go and toolchain lines of go.mod: the Go release the
// in-jail binaries are built with.
func goModPins(t *testing.T, root string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var pins []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "go ") || strings.HasPrefix(l, "toolchain ") {
			pins = append(pins, l)
		}
	}
	return pins
}

// canonicalGo is a Go file reduced to what reaches the binary: comments
// dropped and gofmt spacing, so a comment edit needs no bump. Line breaks
// survive (the printer keeps them), so reflowing code does count. Compiler
// directives (//go:build, //go:embed, //go:linkname, ...) are comments to the
// parser but change the binary, so they are kept, in order, after the code.
func canonicalGo(path string, src []byte) ([]byte, error) {
	var directives []string
	for _, l := range strings.Split(string(src), "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "//go:") {
			directives = append(directives, l)
		}
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	blankVersion(f)
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, err
	}
	for _, d := range directives {
		buf.WriteString(d + "\n")
	}
	return buf.Bytes(), nil
}

// blankVersion empties a top-level string constant named Version: the
// release constant in internal/cli/root.go. lever-manager links that package,
// but a release bump alone changes no in-jail behaviour (that is the point of
// JailABI).
func blankVersion(f *ast.File) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if n.Name != "Version" || i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					lit.Value = `""`
				}
			}
		}
	}
}

// fingerprint hashes the build pins and external modules, then every
// file's path and canonical content, in order.
func fingerprint(t *testing.T, root string, pins, mods, files []string) string {
	t.Helper()
	h := sha256.New()
	for _, l := range append(slices.Clone(pins), mods...) {
		io.WriteString(h, l)
		h.Write([]byte{0})
	}
	h.Write([]byte{0})
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(rel, ".go") {
			if b, err = canonicalGo(rel, b); err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
		}
		var n [8]byte
		io.WriteString(h, rel)
		h.Write([]byte{0})
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type recorded struct {
	abi int
	sum string
}

func readFingerprint(path string) (recorded, error) {
	var r recorded
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		switch k {
		case "jail_abi":
			if r.abi, err = strconv.Atoi(v); err != nil {
				return r, fmt.Errorf("jail_abi: %w", err)
			}
		case "sha256":
			r.sum = v
		}
	}
	if r.abi == 0 || r.sum == "" {
		return r, errors.New("fingerprint.txt lacks a jail_abi or sha256 line")
	}
	return r, sc.Err()
}

func writeFingerprint(path string, pins, pkgs, mods []string, sum string) error {
	var b strings.Builder
	b.WriteString("# The in-jail sources' fingerprint (internal/jailabi). Do not edit by hand.\n")
	b.WriteString("# After changing in-jail code: bump JailABI in jailabi.go, then run\n")
	b.WriteString("#   " + updateCmd + "\n")
	fmt.Fprintf(&b, "jail_abi %d\n", JailABI)
	fmt.Fprintf(&b, "sha256 %s\n", sum)
	b.WriteString("# hashed: go.mod's " + strings.Join(pins, ", ") + ", the files " + strings.Join(jailExtraFiles, ", ") + ",\n")
	b.WriteString("# the external modules\n")
	for _, m := range mods {
		b.WriteString("#   " + m + "\n")
	}
	b.WriteString("# and the packages\n")
	for _, p := range pkgs {
		b.WriteString("#   " + p + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// TestJailFingerprint fails when the code baked into the agent image changed
// without a JailABI bump (lever#171). See the package doc.
func TestJailFingerprint(t *testing.T) {
	root := repoRoot(t)
	pkgs, mods, files := jailSources(t, root)
	pins := goModPins(t, root)
	sum := fingerprint(t, root, pins, mods, files)
	const path = "fingerprint.txt"

	rec, readErr := readFingerprint(path)
	if *update {
		if readErr == nil && rec.sum != sum && rec.abi >= JailABI {
			t.Fatalf("in-jail code changed but JailABI is still %d (fingerprint.txt records %d): "+
				"bump JailABI in internal/jailabi/jailabi.go first, then run %s", JailABI, rec.abi, updateCmd)
		}
		if err := writeFingerprint(path, pins, pkgs, mods, sum); err != nil {
			t.Fatal(err)
		}
		return
	}
	if readErr != nil {
		t.Fatalf("read %s: %v (run %s)", path, readErr, updateCmd)
	}
	if rec.sum != sum {
		t.Fatalf("the in-jail code (lever-agent, lever-manager, lever-tool-db, a package or module they import, the pre-start hook, the image Dockerfile or the Go version) "+
			"changed since fingerprint.txt was recorded: bump JailABI and update the fingerprint (%s)", updateCmd)
	}
	if rec.abi != JailABI {
		t.Fatalf("JailABI is %d but fingerprint.txt records %d: run %s", JailABI, rec.abi, updateCmd)
	}
}

// TestJailFingerprintIgnoresCommentsAndVersion: the canonical form drops
// what never reaches a binary — comments, layout and the release constant —
// and keeps code.
func TestJailFingerprintIgnoresCommentsAndVersion(t *testing.T) {
	base := "package p\n\nconst Version = \"0.34.1\"\n\nfunc F() int { return 1 }\n"
	same := []string{
		"package p\n\n// F is one.\nconst Version = \"0.34.2\"\n\nfunc F() int { return 1 } // one\n",
		"package p\n\nconst   Version = \"0.35.0\"\n\n/* F */ func F() int { return 1 }\n",
	}
	diff := []string{
		"//go:build ignore\n\npackage p\n\nconst Version = \"0.34.1\"\n\nfunc F() int { return 1 }\n",
		"package p\n\nconst Version = \"0.34.1\"\n\nfunc F() int { return 2 }\n",
		"package p\n\nconst Version2 = \"0.34.1\"\n\nfunc F() int { return 1 }\n",
	}
	want, err := canonicalGo("p.go", []byte(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range same {
		got, err := canonicalGo("p.go", []byte(s))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("canonical form differs for %q:\n%s\nvs\n%s", s, got, want)
		}
	}
	for _, s := range diff {
		got, err := canonicalGo("p.go", []byte(s))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(got, want) {
			t.Errorf("canonical form unchanged for %q", s)
		}
	}
}

// TestJailABIReadableByMake: the Makefile reads JailABI with sed for the
// image label, so the declaration must keep the exact line shape it matches.
func TestJailABIReadableByMake(t *testing.T) {
	src, err := os.ReadFile("jailabi.go")
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("\nconst JailABI = %d\n", JailABI)
	if !strings.Contains(string(src), line) {
		t.Fatalf("jailabi.go lacks the line %q the Makefile's sed reads", strings.TrimSpace(line))
	}
	mk, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mk), `s/^const JailABI = \([0-9]*\)$$/\1/p' internal/jailabi/jailabi.go`) {
		t.Fatal("Makefile no longer reads JailABI from internal/jailabi/jailabi.go")
	}
}
