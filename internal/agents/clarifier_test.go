package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleQuestionGraphJSON = `{
  "questions": [
    {"id": "q1", "text": "What latency target matters?", "depends_on": []},
    {"id": "q2", "text": "Is the data partitionable?", "depends_on": []},
    {"id": "q3", "text": "Given q1, q2, which storage backend?", "depends_on": ["q1", "q2"]}
  ]
}`

func TestParseQuestionGraphHappyPath(t *testing.T) {
	g, err := ParseQuestionGraph(sampleQuestionGraphJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Questions) != 3 {
		t.Errorf("got %d questions, want 3", len(g.Questions))
	}
	if g.Questions[2].ID != "q3" || len(g.Questions[2].DependsOn) != 2 {
		t.Errorf("q3 shape wrong: %+v", g.Questions[2])
	}
}

func TestParseQuestionGraphWithCodeFence(t *testing.T) {
	wrapped := "```json\n" + sampleQuestionGraphJSON + "\n```"
	g, err := ParseQuestionGraph(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Questions) != 3 {
		t.Errorf("got %d questions, want 3", len(g.Questions))
	}
}

func TestParseQuestionGraphEmptyQuestionsAllowed(t *testing.T) {
	g, err := ParseQuestionGraph(`{"questions":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Questions) != 0 {
		t.Errorf("expected empty, got %v", g.Questions)
	}
}

func TestParseQuestionGraphRejectsDuplicateID(t *testing.T) {
	raw := `{"questions":[
    {"id":"q1","text":"a","depends_on":[]},
    {"id":"q1","text":"b","depends_on":[]}
  ]}`
	_, err := ParseQuestionGraph(raw)
	if err == nil {
		t.Error("expected duplicate id error")
	}
}

func TestParseQuestionGraphRejectsUnknownDependency(t *testing.T) {
	raw := `{"questions":[
    {"id":"q1","text":"a","depends_on":["missing"]}
  ]}`
	_, err := ParseQuestionGraph(raw)
	if err == nil {
		t.Error("expected unknown dependency error")
	}
}

func TestParseQuestionGraphRejectsSelfDependency(t *testing.T) {
	raw := `{"questions":[
    {"id":"q1","text":"a","depends_on":["q1"]}
  ]}`
	_, err := ParseQuestionGraph(raw)
	if err == nil {
		t.Error("expected self-dependency error")
	}
}

func TestParseQuestionGraphRejectsCycle(t *testing.T) {
	raw := `{"questions":[
    {"id":"q1","text":"a","depends_on":["q2"]},
    {"id":"q2","text":"b","depends_on":["q1"]}
  ]}`
	_, err := ParseQuestionGraph(raw)
	if err == nil {
		t.Error("expected cycle detection error")
	}
	if err != nil && !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle: %v", err)
	}
}

func TestParseQuestionGraphRejectsEmptyText(t *testing.T) {
	raw := `{"questions":[
    {"id":"q1","text":"","depends_on":[]}
  ]}`
	_, err := ParseQuestionGraph(raw)
	if err == nil {
		t.Error("expected empty text error")
	}
}

func TestBatchByDependenciesIndependent(t *testing.T) {
	g := &QuestionGraph{Questions: []Question{
		{ID: "q1", Text: "a"},
		{ID: "q2", Text: "b"},
		{ID: "q3", Text: "c"},
	}}
	waves := BatchByDependencies(g)
	if len(waves) != 1 {
		t.Errorf("independent questions should fit in 1 wave, got %d", len(waves))
	}
	if len(waves[0]) != 3 {
		t.Errorf("wave 0 should contain all 3 questions, got %d", len(waves[0]))
	}
}

func TestBatchByDependenciesChained(t *testing.T) {
	g := &QuestionGraph{Questions: []Question{
		{ID: "q1", Text: "a"},
		{ID: "q2", Text: "b", DependsOn: []string{"q1"}},
		{ID: "q3", Text: "c", DependsOn: []string{"q2"}},
	}}
	waves := BatchByDependencies(g)
	if len(waves) != 3 {
		t.Errorf("chained questions should produce 3 waves, got %d", len(waves))
	}
	if waves[0][0].ID != "q1" {
		t.Errorf("wave 0 should have q1, got %q", waves[0][0].ID)
	}
	if waves[1][0].ID != "q2" {
		t.Errorf("wave 1 should have q2, got %q", waves[1][0].ID)
	}
	if waves[2][0].ID != "q3" {
		t.Errorf("wave 2 should have q3, got %q", waves[2][0].ID)
	}
}

func TestBatchByDependenciesMixed(t *testing.T) {
	// q1, q2 independent; q3 depends on q1 and q2.
	g := &QuestionGraph{Questions: []Question{
		{ID: "q1", Text: "a"},
		{ID: "q2", Text: "b"},
		{ID: "q3", Text: "c", DependsOn: []string{"q1", "q2"}},
	}}
	waves := BatchByDependencies(g)
	if len(waves) != 2 {
		t.Errorf("should be 2 waves, got %d", len(waves))
	}
	if len(waves[0]) != 2 {
		t.Errorf("wave 0 should have 2 questions (q1, q2), got %d", len(waves[0]))
	}
	if len(waves[1]) != 1 || waves[1][0].ID != "q3" {
		t.Errorf("wave 1 should have q3, got %v", waves[1])
	}
}

func TestBatchByDependenciesEmptyGraph(t *testing.T) {
	waves := BatchByDependencies(&QuestionGraph{})
	if waves != nil {
		t.Errorf("empty graph should produce no waves, got %v", waves)
	}
}

func TestBatchByDependenciesDeterministic(t *testing.T) {
	g := &QuestionGraph{Questions: []Question{
		{ID: "q_c", Text: "c"},
		{ID: "q_a", Text: "a"},
		{ID: "q_b", Text: "b"},
	}}
	waves := BatchByDependencies(g)
	if len(waves[0]) != 3 {
		t.Fatal("one wave with three questions")
	}
	// Sorted by ID — not insertion order.
	want := []string{"q_a", "q_b", "q_c"}
	for i, w := range want {
		if waves[0][i].ID != w {
			t.Errorf("waves[0][%d] = %q, want %q", i, waves[0][i].ID, w)
		}
	}
}

func TestWriteClarifierMarkdownCreatesFile(t *testing.T) {
	dir := t.TempDir()
	g := &QuestionGraph{Questions: []Question{
		{ID: "q1", Text: "latency?"},
		{ID: "q2", Text: "storage?", DependsOn: []string{"q1"}},
	}}
	answers := []Answer{
		{ID: "q1", Text: "under 50 ms"},
		{ID: "q2", Text: "postgres"},
	}
	path, err := WriteClarifierMarkdown(dir, g, answers)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "clarifier.md" {
		t.Errorf("basename = %q", filepath.Base(path))
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "Wave 1") || !strings.Contains(s, "Wave 2") {
		t.Errorf("file missing wave headings: %q", s)
	}
	if !strings.Contains(s, "under 50 ms") || !strings.Contains(s, "postgres") {
		t.Errorf("file missing answers: %q", s)
	}
	if !strings.Contains(s, "Depends on: q1") {
		t.Errorf("file missing dependency annotation: %q", s)
	}
}

func TestWriteClarifierMarkdownHandlesUnansweredQuestions(t *testing.T) {
	dir := t.TempDir()
	g := &QuestionGraph{Questions: []Question{
		{ID: "q1", Text: "?"},
	}}
	path, err := WriteClarifierMarkdown(dir, g, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "(unanswered)") {
		t.Errorf("missing unanswered marker: %q", data)
	}
}
