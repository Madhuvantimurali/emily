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

	send_mu		*sync.Mutex
}

func (email *account) sendMail(rcvrs []string, pld []byte) error {
	auth := smtp.PlainAuth("", email.uname, email.password, email.host)
		// Note that this fails w/o TLS

	return smtp.SendMail(email.host+":"+strconv.FormatUint(email.smtpPort, 10), auth, email.uname, rcvrs, pld)
}

func (email *account) receiveMail(last_fetched uint32) ([][]byte, error) {
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
					res = append(res, b)
				}
			}
		}

		if err := <-done; err != nil {
			return nil, err
		}
	}

	return res, nil
}
