package fizzytool

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testBoard = "03gyvmtu0lb2osl1x3h5hkeii"

// fakeFizzy writes a shell script that records argv, cwd, HOME and the
// token to a log (the path is baked in: the tool passes no other env), copies
// any --body_file/--description_file content into the log, and answers with
// canned JSON. Card 99 is on another board; column list has one column.
func fakeFizzy(t *testing.T) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
LOG='` + logPath + `'
{ printf 'ARGV'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'
  printf 'CWD %s\nHOME %s\nTOKEN %s\nACCOUNT %s\nAPI %s\nFAKEVAR %s\n' "$(pwd -P)" "$HOME" "$FIZZY_TOKEN" "$FIZZY_ACCOUNT" "$FIZZY_API_URL" "$FAKE_VAR"
  for a in "$@"; do case "$a" in --body_file=*|--description_file=*) printf 'FILE '; cat "${a#*=}"; printf '\n';; esac; done
} >> "$LOG"
case "$1 $2" in
"card show")
  if [ "$3" = "99" ]; then b=otherboard0000000000000; else b=` + testBoard + `; fi
  printf '{"ok":true,"data":{"number":%s,"board":{"id":"%s"},"title":"t"}}\n' "$3" "$b" ;;
"column list") printf '{"ok":true,"data":[{"id":"col00000000001","name":"In progress"}]}\n' ;;
"card list")
  case "$*" in
  *--search=mixed*) printf '{"ok":true,"data":[{"number":17,"title":"mine","board":{"id":"` + testBoard + `"}},{"number":99,"board":{"id":"otherboard0000000000000"}},{"number":5}]}\n' ;;
  *) printf '{"ok":true,"data":[{"number":17,"board":{"id":"` + testBoard + `"}}]}\n' ;;
  esac ;;
"comment list") printf '{"ok":true,"data":[]}\n' ;;
"fail now") printf '{"ok":false,"error":"boom tok_SECRET"}\n'; exit 1 ;;
"version "|"version --agent") printf '{"ok":true,"data":{"version":"4.0.1"}}\n' ;;
"big out") head -c 3000000 /dev/zero | tr '\0' 'a'; printf '\n' ;;
*) printf '{"ok":true,"data":{}}\n' ;;
esac
`
	bin = filepath.Join(dir, "fizzy")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func readLog(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func newCLI(t *testing.T, bin string) CLI {
	t.Helper()
	home, work := t.TempDir(), t.TempDir()
	return CLI{Bin: bin, Account: "6182510", Token: "tok_SECRET", Home: home, Work: work, APIURL: "https://app.fizzy.do", Timeout: 10 * time.Second, MaxOut: 1 << 20}
}
