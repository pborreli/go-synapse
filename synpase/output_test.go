package synapse

import (
	"testing"
)

func TestCreateOutput(t *testing.T) {
	fileO := CreateOutput("file", "/la/bas/file.test", false, false, false, nil, nil, "", nil, "", 0, "", 0, "", nil)
	if fileO.GetType() != "file" {
		t.Error("Expected Output to have 'file' type, got ", fileO.GetType())
	}
	fileO = CreateOutput("X-file", "/la/bas/file.test", false, false, false, nil, nil, "", nil, "", 0, "", 0, "", nil)
	if fileO != nil {
		t.Error("Expected Output to be nil cause of unknown type(X-file), got a real Output of type ", fileO.GetType())
	}
}