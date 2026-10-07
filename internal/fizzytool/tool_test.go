package fizzytool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
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

func TestListCardsDropsOtherBoards(t *testing.T) {
	tl, _ := newTool(t)
	res, err := tl.ListCards(context.Background(), "m", "", "mixed", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := res.(json.RawMessage)
	if !ok {
		t.Fatalf("want json.RawMessage, got %T", res)
	}
	var cards []map[string]any
	if err := json.Unmarshal(raw, &cards); err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0]["number"] != float64(17) || cards[0]["title"] != "mine" {
		t.Fatalf("want only card 17 with its fields kept, got %s", raw)
	}
}

func TestMoveCardRefusalListsColumns(t *testing.T) {
	tl, _ := newTool(t)
	_, err := tl.MoveCard(context.Background(), "m", "17", "zzzzzzzzzzzzz")
	if err == nil || !strings.Contains(err.Error(), "col00000000001") || !strings.Contains(err.Error(), "In progress") {
		t.Fatalf("refusal must list column ids and names, got %v", err)
	}
}

func TestCommentRefusesAttachmentMarkup(t *testing.T) {
	tl, logPath := newTool(t)
	ctx := context.Background()
	body := `ping <action-text-attachment sgid="BAh7" content-type="application/vnd.actiontext.mention"></action-text-attachment>`
	if _, err := tl.Comment(ctx, "m", "17", body); err == nil || !strings.Contains(err.Error(), "action-text-attachment") {
		t.Fatalf("comment: want the markup refusal, got %v", err)
	}
	if _, err := tl.CreateCard(ctx, "m", "t", `<figure data-trix-attachment='{"sgid":"x"}'></figure>`); err == nil {
		t.Fatal("create_card: want the markup refusal")
	}
	if log := readLog(t, logPath); log != "" {
		t.Fatalf("a refused body must not reach the CLI:\n%s", log)
	}
}

func TestWriteRateLimit(t *testing.T) {
	tl, logPath := newTool(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tl.Now = func() time.Time { return now }
	tl.WriteLimit = 3
	for i := range 2 {
		if _, err := tl.Comment(ctx, "m", "17", "hi"); err != nil {
			t.Fatalf("comment %d: %v", i, err)
		}
	}
	if _, err := tl.CreateCard(ctx, "m", "t", "d"); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := tl.Comment(ctx, "m", "17", "hi")
	if err == nil || !strings.HasPrefix(err.Error(), "rate") {
		t.Fatalf("4th write in the minute: want a rate refusal, got %v", err)
	}
	if _, err := tl.CreateCard(ctx, "m", "t", "d"); err == nil || !strings.HasPrefix(err.Error(), "rate") {
		t.Fatalf("create over the cap: want a rate refusal, got %v", err)
	}
	if got := strings.Count(readLog(t, logPath), "[create]"); got != 3 {
		t.Fatalf("want 3 creates sent, got %d", got)
	}
	// Reads are not limited.
	if _, err := tl.ShowCard(ctx, "m", "17"); err != nil {
		t.Fatalf("show over the write cap: %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := tl.Comment(ctx, "m", "17", "hi"); err != nil {
		t.Fatalf("after a minute the cap must reset: %v", err)
	}
}

func TestWriteRateLimitDefault(t *testing.T) {
	tl, _ := newTool(t)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tl.Now = func() time.Time { return now }
	for i := range DefaultWriteLimit {
		if err := tl.takeWrite(); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := tl.takeWrite(); err == nil {
		t.Fatalf("write %d must be refused", DefaultWriteLimit+1)
	}
}
