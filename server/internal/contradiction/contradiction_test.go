package contradiction

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type stubClient struct {
	reply string
	err   error
	gotReq ChatRequest
}

func (c *stubClient) ChatJSON(_ context.Context, req ChatRequest) (string, error) {
	c.gotReq = req
	return c.reply, c.err
}

func TestCheckerReturnsNilWhenDisabled(t *testing.T) {
	c := &Checker{Disabled: true, Client: &stubClient{}}
	notes, err := c.Check(context.Background(), Input{
		Researchers: []ResearcherView{{Role: "bull", Body: "a"}, {Role: "bear", Body: "b"}},
	})
	if err != nil {
		t.Fatalf("disabled checker should not error, got %v", err)
	}
	if notes != nil {
		t.Fatalf("expected nil notes when disabled, got %v", notes)
	}
}

func TestCheckerSkipsSingleResearcher(t *testing.T) {
	c := New(&stubClient{reply: `{"notes":[]}`})
	notes, err := c.Check(context.Background(), Input{
		Researchers: []ResearcherView{{Role: "bull", Body: "a"}},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if notes != nil {
		t.Fatalf("expected nil for single researcher, got %v", notes)
	}
}

func TestCheckerSwallowsLLMError(t *testing.T) {
	c := New(&stubClient{err: errors.New("rate limited")})
	notes, err := c.Check(context.Background(), Input{
		Researchers: []ResearcherView{{Role: "bull", Body: "a"}, {Role: "bear", Body: "b"}},
	})
	if err == nil {
		t.Fatal("expected LLM error to bubble up")
	}
	if notes != nil {
		t.Fatalf("expected nil notes on error, got %v", notes)
	}
}

func TestCheckerSwallowsGarbageJSON(t *testing.T) {
	c := New(&stubClient{reply: "this is not json"})
	notes, err := c.Check(context.Background(), Input{
		Researchers: []ResearcherView{{Role: "bull", Body: "a"}, {Role: "bear", Body: "b"}},
	})
	if err != nil {
		t.Fatalf("garbage JSON should be silently dropped, got %v", err)
	}
	if notes != nil {
		t.Fatalf("expected nil notes on parse failure, got %v", notes)
	}
}

func TestCheckerParsesEnvelopeNotes(t *testing.T) {
	c := New(&stubClient{reply: `{"notes":[
        {"severity":"warning","summary":"bull and bear disagree on AAPL direction","evidence":"bull: BUY AAPL; bear: SHORT AAPL","symbol":"AAPL"},
        {"severity":"info","summary":"slight tone difference"}
    ]}`})
	notes, err := c.Check(context.Background(), Input{
		TradingDate: time.Now(),
		Researchers: []ResearcherView{
			{Role: "bull", Body: "BUY AAPL strong"},
			{Role: "bear", Body: "SHORT AAPL"},
		},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("want 2 notes, got %d", len(notes))
	}
	if notes[0].Symbol != "AAPL" || notes[0].Severity != SeverityWarning {
		t.Fatalf("first note bad: %+v", notes[0])
	}
}

func TestCheckerHonoursMaxNotes(t *testing.T) {
	c := New(&stubClient{reply: `{"notes":[
        {"severity":"warning","summary":"a"},
        {"severity":"warning","summary":"b"},
        {"severity":"warning","summary":"c"},
        {"severity":"warning","summary":"d"}
    ]}`})
	c.MaxNotes = 2
	notes, _ := c.Check(context.Background(), Input{
		Researchers: []ResearcherView{{Role: "bull", Body: "x"}, {Role: "bear", Body: "y"}},
	})
	if len(notes) != 2 {
		t.Fatalf("MaxNotes ignored: got %d notes", len(notes))
	}
}

func TestFormatRiskNotesFiltersInfo(t *testing.T) {
	notes := []Note{
		{Severity: SeverityInfo, Summary: "noise"},
		{Severity: SeverityWarning, Summary: "real", Symbol: "AAPL"},
	}
	out := FormatRiskNotes(notes)
	if len(out) != 1 {
		t.Fatalf("expected 1 risk note, got %d (%v)", len(out), out)
	}
	if !strings.Contains(out[0], "[contradiction]") {
		t.Fatalf("missing prefix: %s", out[0])
	}
	if !strings.Contains(out[0], "AAPL") {
		t.Fatalf("missing symbol: %s", out[0])
	}
}

func TestNoteStringWithoutSymbol(t *testing.T) {
	n := Note{Severity: SeverityBlock, Summary: "macroscopic crash mismatch"}
	s := n.String()
	if !strings.Contains(s, "[BLOCK]") {
		t.Fatalf("expected severity tag, got %q", s)
	}
}

func TestSanitizeNotesNormalisesSeverity(t *testing.T) {
	got := sanitizeNotes([]Note{
		{Severity: "weird", Summary: "x"},
		{Summary: ""},
	})
	if len(got) != 1 {
		t.Fatalf("want 1 cleaned note, got %d", len(got))
	}
	if got[0].Severity != SeverityWarning {
		t.Fatalf("unknown severity should map to warning, got %s", got[0].Severity)
	}
}

func TestParseNotesAcceptsArrayShape(t *testing.T) {
	body := `[{"severity":"warning","summary":"hello"}]`
	notes, err := parseNotes(body)
	if err != nil {
		t.Fatalf("parse array: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("expected 1, got %d", len(notes))
	}
}

func TestParseNotesStripsCodeFence(t *testing.T) {
	body := "```json\n{\"notes\":[{\"severity\":\"warning\",\"summary\":\"hi\"}]}\n```"
	notes, err := parseNotes(body)
	if err != nil {
		t.Fatalf("parse code-fenced: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("expected 1, got %d", len(notes))
	}
}
