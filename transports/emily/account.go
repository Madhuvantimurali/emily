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
	
}
