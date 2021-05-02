//
//	Golang implementation of Raven
//
//	Eliana Troper

package emily

import (
	"io"
	"io/ioutil"

	"time"
	"fmt"
	"os"
	"sync"
	"bytes"
	"strconv"

	"net/smtp"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"

	"encoding/csv"
	"encoding/binary"

	"github.com/google/uuid"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
)

type Conn struct {
	// net.Conn NEXT
	account		*account

	model_path	string

	queue		[]*message

	lastMessage	uint32
	slots		[]slot

	is_sent		map[uuid.UUID]bool
	sent_mu		*sync.Mutex
}

func NewConn(account *account, model_path string) (*Conn, error) {
	// URGENT: If there is an account, get it
	res := {
		account:	account,

		model_path:	model_path,

		queue:		make([]*message, 0),

		lastMessage:	0, // XXX: This should be done better

		slots:		make([]slot, 0),

		is_sent:	make(map[uuid.UUID]bool),
		sent_mu:	&sync.Mutex{},
	}

	err := conn.load_slots(30) // XXX: Arbitrary init amount
	if err != nil {
		return nil, err
	}
	return res, nil
}

type message struct {
	rcvrs		[]string
	msg		[]byte
	uuid		[16]byte
	sent_frags	uint64
}

type slot struct {
	time	time.Time
	size	int
	rcvr_ct	int
}

func (c *Conn) load_slots(num int) error {
	f, err := os.Open(c.model_path) // XXX: Assumes model is sorted chronologically
	if err != nil {
		return err
	}

	r := csv.NewReader(f)
	added := 0
	for added < num {
		// This could be optimized by not reading through every line
		e, err := r.Read()
		if err == io.EOF {
			return fmt.Errorf("load_slots: ran out of slots")
		}

		s_time, err := strconv.ParseInt(e[0], 10, 64)
		if err != nil {
			return err
		}
		size, err := strconv.Atoi(e[1])
		if err != nil {
			return err
		}
		rcvr_ct, err := strconv.Atoi(e[2])
		if err != nil {
			return  err
		}

		s := slot{
			time:		time.Unix(s_time, 0),
			size:		size,
			rcvr_ct:	rcvr_ct,
		}

		if s.time.After(time.Now()) {
			if len(c.slots) == 0 || s.time.After(c.slots[len(c.slots)-1].time) {
				c.slots = append(c.slots, s)
				added += 1
			}
		}
	}
	return nil
}

func (usr *account) check_sent(id uuid.UUID, remove_if_sent bool) (bool, error) {
	sent, ok := usr.is_sent[id]
	if !ok {
		return false, fmt.Errorf("Message not sent, or to be sent")
	}
	if sent && remove_if_sent {
		usr.sent_mu.Lock()
		defer usr.sent_mu.Unlock()
		delete(usr.is_sent, id)
	}
	return sent, nil
}

func (usr *account) rcv() ([][]byte, error) {
	c, err := client.DialTLS(usr.host+":"+strconv.FormatUint(usr.imapPort, 10), nil) // XXX: Not configuring TLS Config
	if err != nil {
		return nil, err
	}

	if err = c.Login(usr.uname, usr.password); err != nil {
		return nil, err
	}

	// TEST: Goes for inbox
	mbox, err := c.Select("INBOX", false)
	if err != nil {
		return nil, err
	}

	res := make([][]byte, 0)

	if mbox.Messages > usr.lastMessage {
		seqset := new(imap.SeqSet)
		seqset.AddRange(usr.lastMessage + 1, mbox.Messages)

		var section *imap.BodySectionName
		items := []imap.FetchItem{section.FetchItem()}

		messages := make(chan *imap.Message, 10)
		done := make(chan error, 1)
		go func() {
			done <- c.Fetch(seqset, items,  messages)
		}()

		for recv := range messages {
			parsed := recv.GetBody(section)
			if parsed == nil {
				return nil, fmt.Errorf("Server didn't returned message body")
			}

			mr, err := mail.CreateReader(parsed)
			if err != nil {
				return nil, err
			}
			// See https://github.com/emersion/go-imap/wiki/Fetching-messages#fetching-the-whole-message-body
			for {
				p, err := mr.NextPart()
				if err == io.EOF {
					break
				} else if err != nil {
					return nil, err
				}

				switch p.Header.(type) {
				case *mail.InlineHeader:
					b, _ := ioutil.ReadAll(p.Body)
					d, err := usr.decrypt(b)
					if d != nil {
						res = append(res, d)
					} else if err != nil && err != io.EOF {
						return nil, err
					}
				}
			}
		}

		if err := <-done; err != nil {
			return nil, err
		}
	}

	return res, nil
}

func (c *Conn) Write(rcvrs []string, b []byte) (err error) {
	// NEXT: Bring in line with net.Conn.Write
	msg := new(message)
	msg.rcvrs = rcvrs
	msg.msg = b
	msg.uuid, err = uuid.NewRandom()
	if err != nil {
		return err
	}
	msg.sent_frags = 0
	c.queue = append(usr.queue, msg)

	func() {
		c.sent_mu.Lock()
		defer c.sent_mu.Unlock()
		c.is_sent[msg.uuid] = false
	}()

	for true { // XXX: Need to ensure something is writing
		sent, err := connection.check_sent(msg.uuid, true)
		if err != nil {
			return err
		}
		if sent {
			return nil
		}
	}
}

func (usr *account) send() (x bool, err error) { // DOC: What does this bool represent
	usr.send_mu.Lock()
	defer usr.send_mu.Unlock()

	if len(usr.slots) == 0 {
		return false, fmt.Errorf("Out of send slots, add an updated model")
	}
	slot := usr.slots[0]
	if slot.time.Before(time.Now()) {
		if len(usr.queue) > 0 {
			err = usr.sendMsg(slot.size)
		} else {
			err = usr.sendDummy(slot.size)
		}
		if err != nil {
			return false, err
		}


		usr.slots = usr.slots[1:]
		if len(usr.slots) < 10 {
			err = usr.load_slots(10)
			if err != nil {
				return true, err
			}
		}
		return true, nil
	}
	return false, nil
}

func (msg *message) makeChunk(size int) (res []byte, pld_size int, err error) { // DOC: Returns
	// A chunk is
	//	uuid (16 bytes)
	//	frag info (1 byte: 1 bit is_last, 7 bit int)
	//	if is_last:
	//		length (4 bytes)
	//		payload (remaining payload bytes)
	//		padding (size - 16 - 1 - 4 - len(payload))
	//	else:
	//		payload (size - 16 - 1 bytes)

	res = make([]byte, size)

	n := copy(res[0:16], msg.uuid[:])
	if n != 16 {
		return nil, -1, fmt.Errorf("makeChunk: err copying uuid")
	}

	if msg.sent_frags > 127 {
		return nil, -1, fmt.Errorf("makeChunk: index out of range")
	}
	to_pack := msg.sent_frags
	if len(msg.msg) <= size - 16 - 1 - 4 {
		// Pack all
		binary.PutUvarint(res[16:17], to_pack + 128)
		binary.PutUvarint(res[17:21], uint64(len(msg.msg)))
		_ = copy(res[21:21+len(msg.msg)], msg.msg)
		return res, -1, nil
	} else {
		// Pack some
		binary.PutUvarint(res[16:17], to_pack)
		pld_size := size - 16 - 1
		_ = copy(res[17:], msg.msg[:pld_size])
		return res, pld_size, nil
	}
}

func (usr *account) randRcvrs() []string {
	return nil // TODO
}

func (usr *account) sendDummy(size int) (err error) {
	rcvrs := usr.randRcvrs()
	chunk := make([]byte, size) // NEXT: Get PGP overhead to reduce
	m, err := usr.encrypt(chunk)
	if err != nil {
		return err
	}
	err = usr.account.sendMail(rcvrs, m)
	return  err
}

func (usr *account) decrypt(raw []byte) ([]byte, error) {

	buff := bytes.NewBuffer(raw)

	block, err := armor.Decode(buff)
	if err != nil {
		return nil, err
	}
	if block.Type != "PGP MESSAGE" { // NEXT: Make this a const
		return nil, io.EOF
	}

	failed := false
	prompt := func(keys []openpgp.Key, symmetric bool) ([]byte, error) {
		if failed {
			return nil, fmt.Errorf("decryption failed")
		}
		failed = true
		return []byte("raven_is_cool"), nil
	}

	md, err := openpgp.ReadMessage(block.Body, nil, prompt, nil)
	if err != nil {
		return nil, fmt.Errorf("Failed decrypting: ", err) // TODO: Turn into EOF
	}

	res, err := ioutil.ReadAll(md.UnverifiedBody)
	if err != nil {
		return nil, fmt.Errorf("Failed parsing: ", err)
	}
	return res, nil
}

func (usr *account) encrypt(msg []byte) ([]byte, error) {

	buf := new(bytes.Buffer)
	armor_w, err := armor.Encode(buf, "PGP MESSAGE", nil) // XXX: May want to do headers
	if err != nil {
		return nil, err
	}
	defer armor_w.Close()

	w, err := openpgp.SymmetricallyEncrypt(armor_w, []byte("raven_is_cool"), nil, nil)
	if err != nil {
		return nil, err
	}
	defer w.Close()

	_, err = w.Write(msg)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (usr *account) sendMsg(size int) (err error) {
	msg := usr.queue[0]
	chunk, pld_size, err := msg.makeChunk(size) // NEXT: Get PGP overhead to reduce
	if err != nil {
		usr.queue = usr.queue[1:] // XXX: This ain't the best, should catch this before enqueueing
		return err
	}
	m, err := usr.encrypt(chunk)
	if err != nil {
		return err
	}
	err = usr.account.sendMail(msg.rcvrs, m)
	if err != nil {
		return err
	}
	if pld_size >= 0 { // If we didn't send the remainder of the message
		msg.msg = msg.msg[pld_size:]
		msg.sent_frags += 1
	} else {
		usr.queue = usr.queue[1:]
		usr.sent_mu.Lock()
		defer usr.sent_mu.Unlock()
		usr.is_sent[msg.uuid] = true
	}
	return nil
}
