package emily

import (
	"testing"

	"bytes"
)

func TestEncryptDecrypt(t *testing.T) {
	b := []byte("Cool message")
	e, err := encrypt(b)
	if err != nil {
		t.Errorf("Encountered encryption error: %s", err)
	} else {
		t.Logf("Encrypted as:\n%s",e)
		d, err := decrypt(e)
		if err != nil {
			t.Errorf("Encountered decryption error: %s", err)
		} else {
			if !bytes.Equal(b, d) {
				t.Fatalf("Combined error, wanted: %v, got: %v", b, d)
			}
		}
	}
}
