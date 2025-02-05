//
//	Golang implementation of Raven
//	
//	Mostly, but not completely, conformant to the paper
//	(Hence the different name)
//
//	Eliana Troper and Micah Sherr
//
// TODO: maybe take a look at https://github.com/emersion/go-pgpmail

package emily

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/crypto/ripemd160"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
	"github.com/google/uuid"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
)

// some protections
var check_mu sync.Mutex
var send_mu sync.Mutex
var sent_mu sync.Mutex
var imap_create_mu sync.Mutex
var smtp_create_mu sync.Mutex

// TODO: Adjust init

type Account struct {
	smtpHost string
	smtpPort uint64
	imapHost string
	imapPort uint64
	uname    string
	password string

	Queue []*message // TODO: Replace with a len getter

	slot_chan chan slot

	is_sent map[uuid.UUID]bool

	// a map, keyed by message UUID, of message groups. (a `msg_grp` is a slice
	// of fragments (chunks) belonging to a single message)
	re_grp map[uuid.UUID]*msg_grp

	// really bad to set this to true, will ignore TLS checks
	// (the default is that this is set to false)
	insecure_tls bool

	keyring raven_keyring

	imapClient *client.Client
	smtpClient *smtp.Client
}
func (act *Account) Name() string {
	return act.uname
}

func NewAccount(smtpHost string, smtpPort uint64, imapHost string, imapPort uint64, uname string, password string, keyfile string, insecure_tls bool) (*Account, error) {
	res := &Account{
		smtpHost: smtpHost,
		smtpPort: smtpPort,
		imapHost: imapHost,
		imapPort: imapPort,
		uname:    uname,
		password: password,

		Queue: make([]*message, 0),

		slot_chan: make(chan slot),

		is_sent: make(map[uuid.UUID]bool),

		re_grp: make(map[uuid.UUID]*msg_grp),

		insecure_tls: insecure_tls,

		keyring: loadKeyRing(keyfile),
	}
	go SlotGenerator(res.slot_chan)
	return res, nil
}

type message struct {
	rcvrs      []string
	msg        []byte
	uuid       [16]byte
	sent_frags uint64
}

// a string formatter for messages, to make it pretty
func (m message) String() (res string) {
	hash := sha256.Sum256(m.msg)
	if len(m.msg) < 64 {
		res = fmt.Sprintf("{uuid=%v;recvrs=%v;sent_frags=%v,len=%d,msg_hash=%v,msg=\"%v\"}",
			hex.EncodeToString(m.uuid[:]), m.rcvrs, m.sent_frags, len(m.msg),
			hex.EncodeToString(hash[:]),
			string(m.msg))
	} else {
		// message is fairly big, so just return its hash value
		res = fmt.Sprintf("{uuid=%v;recvrs=%v;sent_frags=%v,len=%d,msg_hash=%v}",
			hex.EncodeToString(m.uuid[:]), m.rcvrs, m.sent_frags, len(m.msg),
			hex.EncodeToString(hash[:]))
	}
	return // returns res
}

type slot struct {
	time    time.Time
	size    int
	rcvr_ct int // MICAH: not sure what this is.  doesn't appear to be used anywhere
}

func (usr *Account) check_sent(id uuid.UUID, remove_if_sent bool) (bool, error) {
	sent, ok := usr.is_sent[id]
	if !ok {
		return false, fmt.Errorf("message not sent, or to be sent")
	}
	if sent && remove_if_sent {
		sent_mu.Lock()
		defer sent_mu.Unlock()
		delete(usr.is_sent, id)
	}
	return sent, nil
}

func (usr *Account) Rcv() ([][]byte, error) {

	var err error

	imap_create_mu.Lock() // only allow one creation of an imap instance
	if usr.imapClient == nil {
		LogDebug("creating new imap instance")
		if usr.insecure_tls {
			LogWarning("using insecure TLS connection")
			tlsconfig := &tls.Config{
				InsecureSkipVerify: true,
				ServerName:         usr.imapHost,
			}
			usr.imapClient, err = client.DialTLS(usr.imapHost+":"+strconv.FormatUint(usr.imapPort, 10), tlsconfig)
		} else {
			usr.imapClient, err = client.DialTLS(usr.imapHost+":"+strconv.FormatUint(usr.imapPort, 10), nil)
		}
		if err != nil {
			LogError("imap.DialTLS error:", err)
			imap_create_mu.Unlock()
			return nil, err
		}
		if err = usr.imapClient.Login(usr.uname, usr.password); err != nil {
			LogError("cannot log in to IMAP server: using username ", usr.uname)
			imap_create_mu.Unlock()
			return nil, err
		}
	} else {
		LogDebug("using existing imap instance")
	}
	imap_create_mu.Unlock()

	// only one check at a time
	check_mu.Lock()
	defer check_mu.Unlock()

	mbox, err := usr.imapClient.Select("INBOX", false)
	if err != nil {
		return nil, err
	}
	LogDebug("mailbox contains ", mbox.Messages, " messages")

	res := make([][]byte, 0) // hold the results

	// fetch only the unread emails
	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{"\\Seen"}
	uids, err := usr.imapClient.Search(criteria)
	if err != nil {
		LogError(err)
	}
	if len(uids) > 0 {
		//uids = uids[0:1]
		LogDebug("grabbing new email with UID=", uids)
		seqset := new(imap.SeqSet)
		seqset.AddNum(uids...)

		var section imap.BodySectionName
		items := []imap.FetchItem{section.FetchItem()}

		messages := make(chan *imap.Message, 1)
		done := make(chan error, 1)
		go func() {
			done <- usr.imapClient.Fetch(seqset, items, messages)
		}()

		var parsed imap.Literal
		for recv := range messages {
			for attempts := 0; attempts < 3; attempts++ {
				parsed = recv.GetBody(&section)
				if parsed == nil {
					LogWarning("IMAP server didn't return message body for msg with uid ", uids, ". Waiting a few ms and will try again.")
					time.Sleep(time.Millisecond * 100)
				} else {
					break
				}
			}
			if parsed == nil {
				LogDebug("IMAP server failed to get body for message:\n", recv)
				return nil, fmt.Errorf("IMAP server didn't return message body")
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
					d, err := decrypt(b, usr.keyring, usr.uname)
					if d != nil {
						r, err := usr.deChunk(d)
						if err != nil {
							return nil, err
						}
						if r != nil {
							res = append(res, r)
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
	LogDebug("rcv() returning with a decrypted message")
	return res, nil
}

func newMessage(b []byte) (msg *message, err error) {
	// Test X
	msg = new(message)
	msg.uuid, err = uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	msg.msg = b
	msg.sent_frags = 0

	return msg, nil
}

/**
 * this is essentially how you send a message -- you enqueue it
 */
func (usr *Account) Enqueue(rcvrs []string, b []byte) (id uuid.UUID, err error) {
	msg, err := newMessage(b)
	if err != nil {
		return uuid.Nil, err
	}
	msg.rcvrs = rcvrs
	usr.Queue = append(usr.Queue, msg)
	sent_mu.Lock()
	defer sent_mu.Unlock()
	usr.is_sent[msg.uuid] = false
	LogDebug("enqueued message with UUID ", hex.EncodeToString(msg.uuid[:]), " and receivers={", rcvrs, "}")
	return msg.uuid, nil
}

func (usr *Account) Send() (err error) {
	send_mu.Lock()
	defer send_mu.Unlock()

	slot := <-usr.slot_chan
	//LogDebug("time now is ", time.Now(), " and slot time is ", slot.time)
	if slot.time.Before(time.Now()) {
		if len(usr.Queue) > 0 {
			err = usr.sendMsg(slot.size)
		} else {
			err = usr.sendDummy(slot.size)
		}
		if err != nil {
			return err
		}
		return nil
	}
	return nil
}

// kinda shocked that int max isn't build in somewhere
func min(x, y int) int {
	if x < y {
		return x
	} else {
		return y
	}
}

/**
Generates a chunk (fragment) to send.  `size` is the max size of the chunk to
send.

Returns the bytes to send including the chunk header and payload (i.e,. the
chunk), the size of that chunk, and (hopefully not) an error.
*/
func (msg *message) makeChunk(chunk_size int) ([]byte, int, error) {
	// Test X

	// A chunk is
	//	uuid (16 bytes)
	//	frag info (1 byte: 1 bit is_last, 7 bit int)
	//	if is_last:
	//		length (4 bytes)
	//		payload (remaining payload bytes)
	//		padding (size - 16 - 1 - 4 - len(payload))
	//	else:
	//		payload (size - 16 - 1 bytes)

	buf := new(bytes.Buffer)
	if n, _ := buf.Write(msg.uuid[:]); n != 16 {
		return nil, -1, fmt.Errorf("makeChunk: err copying uuid")
	}

	if msg.sent_frags > 127 {
		return nil, -1, fmt.Errorf("makeChunk: index out of range")
	}
	if len(msg.msg) <= chunk_size-16-1-4 {
		// Pack all
		frag_info := uint8((1 << 7) + msg.sent_frags)
		buf.WriteByte(frag_info)
		length := uint32(len(msg.msg))
		binary.Write(buf, binary.LittleEndian, length)
		buf.Write(msg.msg)
		return buf.Bytes()[:], -1, nil
	} else {
		// Pack some
		length := len(msg.msg)
		pld_size := min(length, chunk_size)
		frag_info := uint8(msg.sent_frags)
		buf.WriteByte(frag_info)
		buf.Write(msg.msg[:pld_size])
		return buf.Bytes()[:], pld_size, nil
	}
}

func (usr *Account) sendDummy(size int) (err error) {
	rcvrs := []string{usr.uname} // we're just gonna send it to ourself
	chunk := make([]byte, size)  // NEXT: Get PGP overhead to reduce
	m, err := encrypt(chunk, "dummy", usr.keyring)
	if err != nil {
		return err
	}
	err = usr.sendMail(rcvrs, m)
	return err
}

func decrypt(raw []byte, keyring raven_keyring, self string) ([]byte, error) {
	// Test X

	pubKey := decodePublicKey(keyring[self])
	privKey := decodePrivateKey(keyring[self])

	entity := createEntityFromKeys(pubKey, privKey)

	buff := bytes.NewBuffer(raw)

	block, err := armor.Decode(buff)
	if err != nil {
		return nil, err
	}
	if block.Type != "PGP MESSAGE" { // NEXT: Make this a const
		return nil, io.EOF
	}

	var entityList openpgp.EntityList
	entityList = append(entityList, entity)

	md, err := openpgp.ReadMessage(block.Body, entityList, nil, nil)
	/*
		failed := false
		prompt := func(keys []openpgp.Key, symmetric bool) ([]byte, error) {
			if failed {
				return nil, fmt.Errorf("decryption failed")
			}
			failed = true
			return []byte(keyring[self].PrivateKey), nil
			//return []byte("raven_is_cool"), nil
		}

		md, err := openpgp.ReadMessage(block.Body, nil, prompt, nil)
	*/
	if err != nil {
		LogInfo("could not decrypt: likely a dummy message: ", err)
		return nil, io.EOF
	} else {
		LogDebug("successfully decrypted GPG message")
	}

	res, err := ioutil.ReadAll(md.UnverifiedBody)
	if err != nil {
		return nil, fmt.Errorf("Failed parsing: %s", err)
	} else {
		LogDebug("read body of decrypted GPG message")
	}

	return res, nil
}

// message fragment
type msg_frg struct {
	frag_num uint // probably should be a uint8
	pld      []byte
}

// A msg_grp is a slice of fragments (chunks) belonging to a single message.
// `last` is -1 when the last frag_num isn't known, or otherwise the last
// frag_num.
type msg_grp struct {
	frgs []*msg_frg
	last int
}

func (grp *msg_grp) String() string {
	frag_num_list := make([]uint, 0)
	for _, frg := range grp.frgs {
		frag_num_list = append(frag_num_list, frg.frag_num)
	}

	return fmt.Sprintf("{msg_grp last=%v; num_frags_received=%d,frags=%v}",
		grp.last, len(grp.frgs), frag_num_list)
}

/**
attempts to construct a (high-level) message (i.e., what Alice wants
to send to Bob) based on a group of received messages (i.e., emails)
*/
func (grp *msg_grp) reconstruct() ([]byte, error) {
	// first step, reassemble fragments in order
	num_frags := len(grp.frgs)
	buf := make([][]byte, num_frags)
	for _, frg := range grp.frgs {
		if frg.frag_num >= uint(num_frags) {
			return nil, fmt.Errorf("last fragment arrived before others -- PROGRAMMING ERROR! :(")
		}
		buf[frg.frag_num] = frg.pld
	}
	// ok, now that we have it actually in order, let's just dump the results to
	// a super big buffer
	res := make([]byte, 0)
	for _, chk := range buf {
		res = append(res, chk...) // append chk slide to res
	}
	return res, nil
}

/*
This function does quite a bit.

it takes a chunk (i.e., the contents of an email), and produces a message
fragment (msg_frg).  It then checks whether that message belongs to an existing
message group (msg_grp).  If it doesn't, it creates one.  Otherwise, it appends
it to that group.

Finally, if all message fragments have arrived, it calls reconstruct() to
reconstruct the final message.
*/
func (usr *Account) deChunk(raw []byte) ([]byte, error) {

	this_fragment := new(msg_frg)

	reader := bytes.NewBuffer(raw)
	id, err := uuid.FromBytes(reader.Next(16)[:])
	if err != nil {
		return nil, err
	}
	// A chunk is
	//	uuid (16 bytes)
	//	frag info (1 byte: 1 bit is_last, 7 bit int)
	//	if is_last:
	//		length (4 bytes)
	//		payload (remaining payload bytes)
	//		padding (size - 16 - 1 - 4 - len(payload))
	//	else:
	//		payload (size - 16 - 1 bytes)

	frag_info, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	is_last := false
	if frag_info >= (1 << 7) {
		is_last = true
		this_fragment.frag_num = uint(frag_info) - (1 << 7)
	} else {
		this_fragment.frag_num = uint(frag_info)
	}
	if !is_last {
		this_fragment.pld = reader.Bytes()[:]
	} else {
		length_packed := reader.Next(4)[:]
		var length uint32
		len_reader := bytes.NewReader(length_packed)
		binary.Read(len_reader, binary.LittleEndian, &length)
		this_fragment.pld = reader.Next(int(length))[:]
	}
	grp, ok := usr.re_grp[id]
	if !ok {
		grp = new(msg_grp)
		grp.last = -1
		usr.re_grp[id] = grp
	}
	grp.frgs = append(grp.frgs, this_fragment)
	if is_last {
		grp.last = int(this_fragment.frag_num)
	}

	// This logic requires some explanation...
	// If (1) we received the last fragment (i.e., grp.last != -1) and (2) the
	// number of fragments we received (len(grp.frgs) equals the last fragment
	// number (minus 1, since we start counting at 0), then we have everything
	// and thus it's safe to reconstruct
	if (grp.last > -1) && (grp.last == (len(grp.frgs) - 1)) {
		LogDebug("received all chunks for msg uid ", hex.EncodeToString(id[:]))
		b, err := grp.reconstruct()
		if err != nil {
			return nil, err
		}
		if b != nil {
			delete(usr.re_grp, id) // XXX: If some dupes arrive later...
		}
		return b, nil
	}
	// if it's not the last fragment, return nil
	LogDebug("received a chunk for msg uid ",
		hex.EncodeToString(id[:]),
		", but haven't received all chunks; current frag group is ",
		grp)
	return nil, nil
}

func encrypt(msg []byte, to string, keyring raven_keyring) ([]byte, error) {

	buf := bytes.NewBuffer(nil)
	armor_w, err := armor.Encode(buf, "PGP MESSAGE", make(map[string]string))
	if err != nil {
		return nil, err
	}

	keypair, ok := keyring[to]
	if !ok {
		LogError("cannot find keypair for recipient: ", to)
		return nil, fmt.Errorf("cannot find keypair for recipient '%v'", to)
	}

	

	pubKey := decodePublicKey(keypair)
	LogDebug(pubKey)
	privKey := decodePrivateKey(keypair)
	dst := createEntityFromKeys(pubKey, privKey)

	w, err := openpgp.Encrypt(armor_w, []*openpgp.Entity{dst}, nil, nil, nil)
	if err != nil {
		LogError("cannot encrypt: ", err)
		return nil, err
	}
	defer w.Close()

	/*
		// TODO: remove this... was for symmetric case
			w, err := openpgp.SymmetricallyEncrypt(armor_w, pass, nil, nil)
			if err != nil {
				return nil, err
			}
			defer w.Close()
	*/

	_, err = w.Write(msg)
	if err != nil {
		return nil, err
	}

	w.Close()
	armor_w.Close()

	return buf.Bytes(), nil
}

/**
 * grabs messages off the queue and sends them
 */
func (usr *Account) sendMsg(size int) (err error) {
	LogDebug("sendMsg: queue has ", len(usr.Queue), " messages")
	msg := usr.Queue[0]
	LogDebug("sendMsg: grabbing message off queue and prepping for send: ", msg, "; slot size is ", size)
	if len(msg.rcvrs) < 1 || len(msg.rcvrs[0]) < 1 {
		LogWarning("sendMsg: no receivers or empty ('') receiver specified, so marking message as being sent")
		// TODO: shouldn't the next line be within a Lock() Unlock() stanza?
		usr.Queue = usr.Queue[1:] // dequeue (i.e., mark message as sent!)
		sent_mu.Lock()
		defer sent_mu.Unlock()
		usr.is_sent[msg.uuid] = true
		return nil
	}
	chunk, pld_size, err := msg.makeChunk(size) // NEXT: Get PGP overhead to reduce
	if err != nil {
		LogWarning("makeChunk returned an error: ", err)
		// TODO: shouldn't the next line be within a Lock() Unlock() stanza?
		usr.Queue = usr.Queue[1:] // XXX: This ain't the best, should catch this before enqueueing
		return err
	}
	m, err := encrypt(chunk, msg.rcvrs[0], usr.keyring)
	if err != nil {
		return err
	}
	err = usr.sendMail(msg.rcvrs, m)
	if err != nil {
		LogError("sendMail returned an error: ", err)
		return err
	} else {
		LogDebug("sent email")
	}
	if pld_size >= 0 { // If we didn't send the remainder of the message
		msg.msg = msg.msg[pld_size:]
		msg.sent_frags += 1
	} else {
		LogDebug("completed sending message with UUID ", hex.EncodeToString(msg.uuid[:]))
		usr.Queue = usr.Queue[1:] // dequeue (i.e., mark message as sent!)
		sent_mu.Lock()
		defer sent_mu.Unlock()
		usr.is_sent[msg.uuid] = true
	}
	return nil
}

/*
	actually use SMTP to send amessage

	`pld` is the actual message to send
*/
func (usr *Account) sendMail(rcvrs []string, pld []byte) error {
	var err error

	smtp_create_mu.Lock()
	if usr.smtpClient == nil {
		auth := smtp.PlainAuth("", usr.uname, usr.password, usr.smtpHost)
		servername := usr.smtpHost + ":" + strconv.FormatUint(usr.smtpPort, 10)

		tlsconfig := &tls.Config{
			InsecureSkipVerify: usr.insecure_tls, // DANGER!
			ServerName:         usr.smtpHost,
		}
		usr.smtpClient, err = smtp.Dial(servername)
		if err != nil {
			LogError("cannot connect (smtp.Dial failed connecting to ", servername, "): ", err)
			smtp_create_mu.Unlock()
			return err
		}
		/*
		if err = usr.smtpClient.Hello(usr.smtpHost); err != nil {
			smtp_create_mu.Unlock()
			return err
		}
		*/
		if err = usr.smtpClient.StartTLS(tlsconfig); err != nil {
			smtp_create_mu.Unlock()
			return err
		}
		if err = usr.smtpClient.Auth(auth); err != nil {
			smtp_create_mu.Unlock()
			return err
		}
	} else {
		// TODO: send a RST
	}
	smtp_create_mu.Unlock()

	// send a message
	receivers_as_string := strings.Join(rcvrs, ",")
	header := []byte("To: " + receivers_as_string + "\r\n" +
		"From: " + usr.uname + "\r\n" +
		"Subject: ...\r\n" +
		"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n" +
		"\r\n")

	if err = usr.smtpClient.Mail(usr.uname); err != nil {
		return err
	}
	for _, receiver := range rcvrs {
		if err = usr.smtpClient.Rcpt(receiver); err != nil {
			return err
		}
	}
	// Data
	w, err := usr.smtpClient.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write([]byte(header)); err != nil {
		return err
	}
	if _, err = w.Write(pld); err != nil {
		return err
	}
	w.Close()

	/*
		// let's go ahead and keep the connection alive.
		if err = client.Quit(); err != nil {
			return err
		}
	*/
	return nil
}
