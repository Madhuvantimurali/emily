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

type account struct {
	host		string
	smtpPort	uint64
	imapPort	uint64
	uname		string
	password	string
	model_path	string // For now, just holds the model

	queue		[]*message

	lastMessage	uint32 // XXX: We assume the user never deletes a single email

	slots		[]slot

	is_sent		map[uuid.UUID]bool
	sent_mu		*sync.Mutex

	send_mu		*sync.Mutex

	re_grp		map[uuid.UUID]*msg_grp
}

func newAccount(host string, smtpPort uint64, imapPort uint64, uname string, password string, model_path string) (*account, error) {
	res := &account {
		host:		host,
		smtpPort:	smtpPort,
		imapPort:	imapPort,
		uname:		uname,
		password:	password,
		model_path:	model_path,

		queue:		make([]*message, 0),

		lastMessage:	0, // XXX: This should probably be done better

		slots:		make([]slot, 0),

		is_sent:	make(map[uuid.UUID]bool),
		sent_mu:	&sync.Mutex{},

		send_mu:	&sync.Mutex{},

		re_grp:		make(map[uuid.UUID]*msg_grp),
	}

	err := res.load_slots(30) // XXX: Arbitrary init amount
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

func (usr *account) load_slots(num int) error {
	f, err := os.Open(usr.model_path) // XXX: Assumes model is sorted chronologically
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
			if len(usr.slots) == 0 || s.time.After(usr.slots[len(usr.slots)-1].time) {
				usr.slots = append(usr.slots, s)
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
					// d, err := usr.decrypt(b) // 2do
					d := b
					if d != nil {
						r, err := usr.deChunk(d)
						if err != nil {
							return nil, err
						}
						if r != nil {
							res = append(res, d)
						}
					} else if err != nil {
						if err == io.EOF {
							continue
						} else {
							return nil, err
						}
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

func (usr *account) enqueue(rcvrs []string, b []byte) (id uuid.UUID, err error) {
	msg := new(message)
	msg.rcvrs = rcvrs
	msg.msg = b
	msg.uuid, err = uuid.NewRandom()
	if err != nil {
		return uuid.Nil, err
	}
	msg.sent_frags = 0
	usr.queue = append(usr.queue, msg)
	usr.sent_mu.Lock()
	defer usr.sent_mu.Unlock()
	usr.is_sent[msg.uuid] = false
	return msg.uuid, nil
}

func (usr *account) send() (err error) { // DOC: What does this bool represent
	usr.send_mu.Lock()
	defer usr.send_mu.Unlock()

	if len(usr.slots) == 0 {
		return fmt.Errorf("Out of send slots, add an updated model")
	}
	slot := usr.slots[0]
	if slot.time.Before(time.Now()) {
		if len(usr.queue) > 0 {
			err = usr.sendMsg(slot.size)
		} else {
			err = usr.sendDummy(slot.size)
		}
		if err != nil {
			return err
		}


		usr.slots = usr.slots[1:]
		if len(usr.slots) < 10 {
			err = usr.load_slots(10)
			if err != nil {
				return err
			}
		}
		return nil
	}
	return nil
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
	m, err := encrypt(chunk) // URGENT: Do this with a bad password
	if err != nil {
		return err
	}
	err = usr.sendMail(rcvrs, m)
	return  err
}

func decrypt(raw []byte) ([]byte, error) {

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
		return nil, fmt.Errorf("Failed decrypting: %s", err) // TODO: Turn into EOF
	}

	res, err := ioutil.ReadAll(md.UnverifiedBody)
	if err != nil {
		return nil, fmt.Errorf("Failed parsing: %s", err)
	}

	return res, nil
}

func f_flag(i []byte) (uint, bool, error) {
	if len(i) != 1 {
		return 0, false, fmt.Errorf("Flag is malformed: bad length")
	}
	r, n := binary.Uvarint(i)
	if n <= 0 {
		return 0, false, fmt.Errorf("Flag is malformed: bad val")
	}
	is_last := false
	if r >= 128 {
		is_last = true
		r -= 128
	}
	return uint(r), is_last, nil
}

type msg_frg struct {
	frag	uint
	pld	[]byte
}

type msg_grp struct {
	frgs	[]*msg_frg
	last	int
}

func (grp *msg_grp) reconstruct() ([]byte) {
	if len(grp.frgs) < grp.last + 1 {
		return nil
	}
	buf := make([][]byte, grp.last)
	for _, frg := range grp.frgs {
		buf[frg.frag] = frg.pld
	}
	res := make([]byte, 0)
	for _, chk := range buf {
		if len(chk) == 0 {
			return nil
		}
		res = append(res, chk...)
	}
	return res
}

// URGENT: Map from id to msg_grp
func (usr *account) deChunk(raw []byte) ([]byte, error) {
	res := new(msg_frg)
	id, err := uuid.FromBytes(raw[0:16])
	if err != nil {
		return nil, err
	}
	frag_flag := raw[16:17]
	is_last := false
	res.frag, is_last, err = f_flag(frag_flag)
	if err != nil {
		return nil, err
	}
	if !is_last {
		res.pld = raw[17:]
	} else {
		length, n := binary.Uvarint(raw[17:21])
		if n <= 0 {
			return nil, fmt.Errorf("Error decoding length")
		}
		res.pld = raw[21:21+length]
	}
	grp, ok := usr.re_grp[id]
	if !ok {
		grp = new(msg_grp)
		grp.last = -1
		usr.re_grp[id] = grp
	}
	grp.frgs = append(grp.frgs, res)
	if is_last {
		grp.last = int(res.frag)
	}
	if grp.last > -1 {
		b := grp.reconstruct()
		if b != nil {
			delete(usr.re_grp, id) // XXX: If some dupes arrive later...
		}
		return b, nil
	}
	return nil, nil
}

func encrypt(msg []byte) ([]byte, error) {

	buf := bytes.NewBuffer(nil)
	armor_w, err := armor.Encode(buf, "PGP MESSAGE", nil) // XXX: May want to do headers
	if err != nil {
		return nil, err
	}

	w, err := openpgp.SymmetricallyEncrypt(armor_w, []byte("raven_is_cool"), nil, nil)
	if err != nil {
		return nil, err
	}
	defer w.Close()

	_, err = w.Write(msg)
	if err != nil {
		return nil, err
	}

	w.Close()
	armor_w.Close()

	return buf.Bytes(), nil
}

func (usr *account) sendMsg(size int) (err error) {
	msg := usr.queue[0]
	chunk, pld_size, err := msg.makeChunk(size) // NEXT: Get PGP overhead to reduce
	if err != nil {
		usr.queue = usr.queue[1:] // XXX: This ain't the best, should catch this before enqueueing
		return err
	}
	m := chunk
	// m, err := usr.encrypt(chunk) // 2DO
	// if err != nil {
	//	return err
	//}
	err = usr.sendMail(msg.rcvrs, m)
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

func (usr *account) sendMail(rcvrs []string, pld []byte) error {
	auth := smtp.PlainAuth("", usr.uname, usr.password, usr.host)
		// Note that this fails w/o TLS.

	return smtp.SendMail(usr.host+":"+strconv.FormatUint(usr.smtpPort, 10), auth, usr.uname, rcvrs, pld)
}
