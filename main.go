package main

import (
	"encoding/json"
	"os/exec"
	"flag"
	"fmt"
	"time"
	"io/ioutil"
	"os"
	"github.com/racecar-gu/emily/lib"
)

var REQUEST = "REQUEST"
var CURL = "/usr/bin/curl"
var URL = "http://128.31.0.34:9131/tor/status-vote/current/consensus"
var CLIENT = "ADDRESS@HOST.NET"
var SERVER = "ADDRESS@HOST.NET"

type email_account struct {
	SmtpHost	string `json:"smtpHost"`
	SmtpPort	uint64 `json:"smtpPort"`
	ImapHost	string `json:"imapHost"`
	ImapPort	uint64 `json:"imapPort"`
	Uname		string `json:"uname"`
	Password	string `json:"password"`
	KeyFilePath	string `json:"keyFilePath"`
}

func send_loop(account *emily.Account) error {
	for len(account.Queue) > 0 {
		err := account.Send()
		if err != nil {
			return err
		}
		time.Sleep(time.Millisecond * 250)
	}
	return nil
}

func rcv_step(account *emily.Account) ([]byte, error) {
	msgs, err := account.Rcv()
	if err != nil {
		return nil, err
	}
	if len(msgs) != 0 {
		for _, data := range msgs {
			return data, nil // XXX: assumes one message at most
		}
	}
	return nil, nil
}

func do_client(account *emily.Account) error {
	// Sends request
	account.Enqueue([]string{SERVER}, []byte(REQUEST))
	err := send_loop(account)
	if err != nil {
		return err
	}
	
	// Waits on reply
	for true {
		msg, err := rcv_step(account)
		if err != nil {
			return err
		}
		if msg != nil {
			emily.LogInfo("Received message: ", string(msg))
			break
		}
		sleep_time := time.Duration(3) * time.Second
		time.Sleep(sleep_time)
	}
	return nil
}

func do_server(account *emily.Account) error {
	// Waits on request
	for true {
		msg, err := rcv_step(account)
		if err != nil {
			return err
		}
		if msg != nil {
			emily.LogInfo("Received message: ", msg)
			break
		}
		sleep_time := time.Duration(3) * time.Second
		time.Sleep(sleep_time)
	}
	
	// Preps reply
	cmd := exec.Command(CURL, URL)
	stdout, err := cmd.Output()
	if err != nil {
		account.Enqueue([]string{CLIENT}, []byte(err.Error()))
	} else {
		account.Enqueue([]string{CLIENT}, stdout)
	}
	
	// Sends reply
	err = send_loop(account)
	if err != nil {
		return err
	}
	
	return nil
}

// --client/--server
// config
func main() {
	// Sets up args
	client := flag.Bool("c", false, "Run as a client")
	server := flag.Bool("s", false, "Run as a server")
	flag.Parse()
	args := flag.Args()
	if len(args) != 1 {
		emily.LogError("Must pass in a config path!")
		os.Exit(1)
	}
	path := args[0]


	// Reads in config
	confFile, err := ioutil.ReadFile(path)
	if err != nil {
		emily.LogError(err)
		os.Exit(1)
	}
	emily.LogInfo(path)
	emily.LogInfo(string(confFile))
	var prep_account email_account
	err = json.Unmarshal(confFile, &prep_account)
	if err != nil {
		emily.LogError(err)
		os.Exit(1)
	}

	emily.LogInfo(prep_account.SmtpHost)
	emily.LogInfo(prep_account.SmtpPort)
	emily.LogInfo(prep_account.ImapHost)
	emily.LogInfo(prep_account.ImapPort)
	emily.LogInfo(prep_account.Uname)
	emily.LogInfo(prep_account.KeyFilePath)

	account, err := emily.NewAccount(prep_account.SmtpHost, prep_account.SmtpPort, prep_account.ImapHost, prep_account.ImapPort, prep_account.Uname, prep_account.Password, prep_account.KeyFilePath)
	if err != nil {
		emily.LogError(err)
		os.Exit(1)
	}

	if *client {
		err = do_client(account)
	} else if *server {
		err = do_server(account)
	} else {
		err = fmt.Errorf("Must run as a client or server!")
	}
	if err != nil {
		emily.LogError(err)
		os.Exit(1)
	}
}
