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
	"go/scanner"
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

type goTok struct {
	kind token.Token
	lit  string
}

// canonicalGo is a Go file reduced to what reaches the binary: its token
// stream, so a comment or layout edit needs no bump. Compiler directives
// (//go:build, //go:embed, ...) are comments to the scanner but change the
// build, so they stay. In internal/cli the release constant Version is
// blanked (see versionValues).
func canonicalGo(path string, src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	file := fset.AddFile(path, -1, len(src))
	var errs scanner.ErrorList
	var s scanner.Scanner
	s.Init(file, src, func(pos token.Position, msg string) { errs.Add(pos, msg) }, scanner.ScanComments)
	var toks []goTok
	for {
		_, kind, lit := s.Scan()
		if kind == token.EOF {
			break
		}
		switch {
		case kind == token.COMMENT && !strings.HasPrefix(lit, "//go:"):
			continue
		case kind == token.SEMICOLON:
			lit = ";"
		case lit == "":
			lit = kind.String()
		}
		toks = append(toks, goTok{kind, lit})
	}
	if errs.Len() > 0 {
		errs.Sort()
		return nil, errs.Err()
	}
	if isVersionPackage(path) {
		for _, i := range versionValues(toks) {
			toks[i].lit = `""`
		}
	}
	var buf bytes.Buffer
	for _, t := range toks {
		fmt.Fprintf(&buf, "%s %s\n", t.kind, strconv.Quote(t.lit))
	}
	return buf.Bytes(), nil
}

func isVersionPackage(path string) bool {
	return filepath.ToSlash(filepath.Dir(path)) == "internal/cli"
}

// versionValues returns the indexes of the STRING tokens that give a
// top-level constant named Version its value, alone or in a const group.
// lever-manager links internal/cli, but a release bump alone changes no
// in-jail behaviour.
func versionValues(toks []goTok) []int {
	var out []int
	depth := 0       // ( [ { nesting
	inConst := false // inside a top-level const ( ... ) group
	specStart := false
	for i := 0; i < len(toks); i++ {
		k := toks[i].kind
		atSpec := specStart
		specStart = false
		switch k {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
			if !inConst && k == token.LPAREN && depth == 1 && i > 0 && toks[i-1].kind == token.CONST {
				inConst = true
				specStart = true
			}
			continue
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if inConst && depth == 1 {
				inConst = false
			}
			depth--
			continue
		case token.SEMICOLON:
			specStart = inConst && depth == 1
			continue
		case token.CONST:
			specStart = depth == 0
			continue
		}
		if !atSpec || k != token.IDENT || toks[i].lit != "Version" {
			continue
		}
		j := i + 1
		if j < len(toks) && toks[j].kind == token.IDENT { // a type name
			j++
		}
		if j+1 < len(toks) && toks[j].kind == token.ASSIGN && toks[j+1].kind == token.STRING {
			out = append(out, j+1)
		}
	}
	return out
}

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
// without a JailABI bump. See the package doc.
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
// what never reaches a binary (comments, blank lines, spacing and, in
// internal/cli, the release constant's value) and keeps code and compiler
// directives.
func TestJailFingerprintIgnoresCommentsAndVersion(t *testing.T) {
	const path = "internal/cli/root.go"
	base := "//go:build linux\n\npackage p\n\nconst Version = \"0.34.1\"\n\nfunc F() int {\n\tx := 1\n\treturn x\n}\n"
	same := map[string]string{
		"doc and trailing comments": "//go:build linux\n\npackage p\n\n// F is one.\nconst Version = \"0.34.2\"\n\nfunc F() int { // one\n\tx := 1 // x\n\treturn x\n} // end\n",
		"standalone comment":        "//go:build linux\n\npackage p\n\nconst Version = \"0.34.1\"\n\nfunc F() int {\n\tx := 1\n\t// now return it\n\treturn x\n}\n",
		"block comment":             "//go:build linux\n\npackage p\n\nconst Version = \"0.34.1\"\n\n/* F */ func F() int {\n\tx := /* one */ 1\n\treturn x\n}\n",
		"blank line":                "//go:build linux\n\npackage p\n\nconst Version = \"0.34.1\"\n\nfunc F() int {\n\tx := 1\n\n\treturn x\n}\n",
		"spacing":                   "//go:build linux\n\npackage p\n\nconst   Version   =   \"0.35.0\"\n\nfunc F()int{\n    x:=1\n    return x\n}\n",
	}
	diff := map[string]string{
		"go:build changed":     "//go:build linux && arm64\n\npackage p\n\nconst Version = \"0.34.1\"\n\nfunc F() int {\n\tx := 1\n\treturn x\n}\n",
		"go:build removed":     "package p\n\nconst Version = \"0.34.1\"\n\nfunc F() int {\n\tx := 1\n\treturn x\n}\n",
		"go:embed added":       "//go:build linux\n\npackage p\n\n//go:embed v.txt\nconst Version = \"0.34.1\"\n\nfunc F() int {\n\tx := 1\n\treturn x\n}\n",
		"code changed":         "//go:build linux\n\npackage p\n\nconst Version = \"0.34.1\"\n\nfunc F() int {\n\tx := 2\n\treturn x\n}\n",
		"constant renamed":     "//go:build linux\n\npackage p\n\nconst Version2 = \"0.34.1\"\n\nfunc F() int {\n\tx := 1\n\treturn x\n}\n",
		"local Version string": "//go:build linux\n\npackage p\n\nconst Version = \"0.34.1\"\n\nfunc F() int {\n\tconst Version = \"x\"\n\tx := 1\n\treturn x\n}\n",
	}
	canon := func(path, src string) []byte {
		t.Helper()
		b, err := canonicalGo(path, []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	want := canon(path, base)
	for name, s := range same {
		if got := canon(path, s); !bytes.Equal(got, want) {
			t.Errorf("%s: canonical form differs for %q:\n%s\nvs\n%s", name, s, got, want)
		}
	}
	for name, s := range diff {
		if bytes.Equal(canon(path, s), want) {
			t.Errorf("%s: canonical form unchanged for %q", name, s)
		}
	}
	// A typed Version, or one in a const group, is blanked too.
	for _, form := range []string{
		"package p\n\nconst Version string = %q\n",
		"package p\n\nconst (\n\tA = 1\n\tVersion = %q\n)\n",
	} {
		if !bytes.Equal(canon(path, fmt.Sprintf(form, "0.34.1")), canon(path, fmt.Sprintf(form, "0.35.0"))) {
			t.Errorf("Version not blanked in %q", form)
		}
	}
	// Outside internal/cli a Version constant is ordinary code.
	other := "internal/agent/p.go"
	if bytes.Equal(canon(other, base), canon(other, same["spacing"])) {
		t.Error("a Version change outside internal/cli left the canonical form unchanged")
	}
	if _, err := canonicalGo(path, []byte("package p\n\nconst s = \"unterminated\n")); err == nil {
		t.Error("canonicalGo accepted a file that does not scan")
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
