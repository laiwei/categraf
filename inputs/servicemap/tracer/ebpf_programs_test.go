package tracer

import "testing"

func TestGetEmbeddedEBPFProgram(t *testing.T) {
	old := embeddedPrograms
	defer func() { embeddedPrograms = old }()

	embeddedPrograms = map[string][]EmbeddedProgram{
		"amd64": {
			{MinKernel: "4.16", Program: []byte("ZmFrZQ==")},
		},
	}

	prog, err := getEmbeddedEBPFProgram("amd64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prog) == 0 {
		t.Fatal("program should not be empty")
	}
}
