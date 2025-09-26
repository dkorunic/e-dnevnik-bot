package db

import (
	"bytes"
	"os"
	"testing"
)

func TestDbExists(t *testing.T) {
	t.Parallel()
	// Test with a file that exists
	tmpfile, err := os.CreateTemp("", "example")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name()) // clean up

	if !dbExists(tmpfile.Name()) {
		t.Errorf("dbExists() = false, want true for existing file")
	}

	// Test with a file that does not exist
	if dbExists("nonexistent-file") {
		t.Errorf("dbExists() = true, want false for nonexistent file")
	}
}

func TestHashContent(t *testing.T) {
	t.Parallel()
	bucket := "test-bucket"
	subBucket := "test-sub-bucket"
	target := []string{"target1", "target2"}

	// Expected hash for the given input
	expectedHash := []byte{0xbb, 0x5e, 0x02, 0x70, 0xda, 0xde, 0x80, 0x51, 0x25, 0x2a, 0xf9, 0x57, 0x43, 0x69, 0x59, 0xe1, 0x69, 0x1e, 0x8a, 0x11, 0x2a, 0xc6, 0x05, 0x0a, 0x52, 0x39, 0x80, 0x99, 0x86, 0xde, 0xa2, 0x39}

	hash := hashContent(bucket, subBucket, target)

	if !bytes.Equal(hash, expectedHash) {
		t.Errorf("hashContent() = %x, want %x", hash, expectedHash)
	}
}
