package emily

import (
	"testing"

	"bytes"
)

func TestEncryptGoodDecrypt(t *testing.T) {
	b := []byte("Cool message")
	e, err := encrypt(b, []byte("raven_is_cool"))
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

func TestEncryptBadDecrypt(t *testing.T) {
	b := []byte("Cool message")
	e, err := encrypt(b, []byte("dummy"))
	if err != nil {
		t.Errorf("Encountered encryption error: %s", err)
	} else {
		t.Logf("Encrypted as:\n%s",e)
		_, err := decrypt(e)
		if err == nil {
			t.Errorf("Succesfully decrypted (which is no bueno!)")
		}
	}
}

func TestMakeChunkDeChunk(t *testing.T) {
	msg, err := newMessage([]byte("FunkySkunkyMonkey"))
	// TODO: Err
	r1, p1, err := msg.makeChunk(22)
	// TODO: Err
	// TODO: Len
	r2, p2, err := msg.makeChunk(23)
	// TODO: Err
	// TODO: Len
	r3, p3, err := msg.makeChunk(42)
	// TODO: Err
	// TODO: Len

	// TODO: Dummy account
	// URGENT
}

// Reconstruct

// check_sent
// enqueue
