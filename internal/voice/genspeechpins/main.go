// Command genspeechpins prints the pinned file list of a speech model for
// lever's table (internal/voice/speechmodels.go) from a local clone of its
// Hugging Face repository checked out at the commit to pin. A maintainer
// tool, run on a machine with internet access; never part of a release.
//
//	git clone https://huggingface.co/csukuangfj/kokoro-en-v0_19 && cd kokoro-en-v0_19
//	git checkout <commit> && git lfs pull
//	go run ./internal/voice/genspeechpins -dir <clone> -revision <commit> -skip 'README.md,*.py'
//
// It lists every regular file below -dir with its size and sha256, in path
// order, except the .git directory, names that start with a dot and the
// -skip patterns (matched against the base name). A symbolic link or an
// LFS pointer left unpulled is refused.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func main() {
	dir := flag.String("dir", "", "local clone of the model repository")
	rev := flag.String("revision", "", "the commit checked out (40 hex)")
	skip := flag.String("skip", "README.md,*.md,*.py,*.sh,LICENSE", "comma list of base-name patterns to leave out")
	flag.Parse()
	if *dir == "" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(*rev) {
		log.Fatal("-dir and a 40-hex -revision are required")
	}
	var pats []string
	for _, p := range strings.Split(*skip, ",") {
		if p = strings.TrimSpace(p); p != "" {
			pats = append(pats, p)
		}
	}
	type entry struct {
		rel  string
		size int64
		sum  string
	}
	var out []entry
	err := filepath.WalkDir(*dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == *dir {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		for _, pat := range pats {
			if ok, _ := path.Match(pat, d.Name()); ok {
				return nil
			}
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		head := make([]byte, 64)
		n, _ := io.ReadFull(f, head)
		if strings.HasPrefix(string(head[:n]), "version https://git-lfs") {
			return fmt.Errorf("%s is an LFS pointer: run git lfs pull", p)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		h := sha256.New()
		size, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(*dir, p)
		if err != nil {
			return err
		}
		out = append(out, entry{filepath.ToSlash(rel), size, hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	var total int64
	fmt.Printf("Revision: %q,\n", *rev)
	fmt.Println("Files: []SpeechFile{")
	for _, e := range out {
		total += e.size
		fmt.Printf("\t{Path: %q, Size: %d, SHA256: %q},\n", e.rel, e.size, e.sum)
	}
	fmt.Println("},")
	fmt.Printf("// %d files, %d bytes in all\n", len(out), total)
}
