package fizzytool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func newTool(t *testing.T) (*Tool, string) {
	t.Helper()
	bin, logPath := fakeFizzy(t)
	return &Tool{CLI: newCLI(t, bin), Board: testBoard, Prefix: "[lever-dev agent] ", TmpDir: t.TempDir(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, logPath
}

func TestBoardCheckRefusesOtherBoard(t *testing.T) {
	tl, logPath := newTool(t)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"show":     func() error { _, err := tl.ShowCard(ctx, "m", "99"); return err },
		"comments": func() error { _, err := tl.ListComments(ctx, "m", "99"); return err },
		"comment":  func() error { _, err := tl.Comment(ctx, "m", "99", "hi"); return err },
		"move":     func() error { _, err := tl.MoveCard(ctx, "m", "99", "col00000000001"); return err },
	} {
		if err := call(); !errors.Is(err, ErrOtherBoard) {
			t.Errorf("%s: want ErrOtherBoard, got %v", name, err)
		}
	}
	log := readLog(t, logPath)
	for _, w := range []string{"[comment] [create]", "[card] [column]", "[comment] [list]"} {
		if strings.Contains(log, w) {
			t.Errorf("a write/read ran for a card on another board: %s", w)
		}
	}
}

func TestCommentAddsPrefixViaFile(t *testing.T) {
	tl, logPath := newTool(t)
	if _, err := tl.Comment(context.Background(), "m", "17", "-- pushed agent/x"); err != nil {
		t.Fatal(err)
	}
	log := readLog(t, logPath)
	if !strings.Contains(log, "FILE [lever-dev agent] -- pushed agent/x") {
		t.Fatalf("body not prefixed or not sent via file:\n%s", log)
	}
	if !strings.Contains(log, "[--card=17]") || strings.Contains(log, "[--body=") {
		t.Fatalf("argv form wrong:\n%s", log)
	}
	entries, _ := os.ReadDir(tl.TmpDir)
	if len(entries) != 0 {
		t.Fatalf("temp body file left behind: %v", entries)
	}
}

func TestMoveCardChecksColumn(t *testing.T) {
	tl, logPath := newTool(t)
	_, err := tl.MoveCard(context.Background(), "m", "17", "zzzzzzzzzzzzz")
	if err == nil || !strings.Contains(err.Error(), "not a column of the board") {
		t.Fatalf("want column refusal, got %v", err)
	}
	if strings.Contains(readLog(t, logPath), "[card] [column]") {
		t.Fatal("move ran for a foreign column")
	}
	if _, err := tl.MoveCard(context.Background(), "m", "17", "col00000000001"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readLog(t, logPath), "[card] [column] [17] [--column=col00000000001]") {
		t.Fatal("move argv wrong")
	}
}

func TestListCardsAndCreateUseBoardAndFlagValueForm(t *testing.T) {
	tl, logPath := newTool(t)
	if _, err := tl.ListCards(context.Background(), "m", "done", "-x --board other", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.CreateCard(context.Background(), "m", "-rf title", "desc"); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.ListCards(context.Background(), "m", "", "", "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.ListCards(context.Background(), "m", "", "", "-1"); err == nil {
		t.Fatal("a non-numeric page must be refused")
	}
	log := readLog(t, logPath)
	for _, w := range []string{"[--board=" + testBoard + "] [--column=done] [--search=-x --board other]", "[--board=" + testBoard + "] [--page=2]", "[card] [create] [--board=" + testBoard + "] [--title=-rf title]", "FILE [lever-dev agent] desc"} {
		if !strings.Contains(log, w) {
			t.Errorf("log missing %q:\n%s", w, log)
		}
	}
}

func TestValidationBeforeAnyCall(t *testing.T) {
	tl, logPath := newTool(t)
	ctx := context.Background()
	_, e1 := tl.ShowCard(ctx, "m", "--help")
	_, e2 := tl.MoveCard(ctx, "m", "17", "done")
	_, e3 := tl.CreateCard(ctx, "m", "a\nb", "")
	_, e4 := tl.Comment(ctx, "m", "17", strings.Repeat("a", 20001))
	for i, e := range []error{e1, e2, e3, e4} {
		if e == nil {
			t.Errorf("case %d: want a refusal", i)
		}
	}
	if log := readLog(t, logPath); log != "" {
		t.Fatalf("no CLI call expected, got:\n%s", log)
	}
}
