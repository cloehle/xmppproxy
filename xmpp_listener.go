// xmpp_listener.go - XMPP listener.
// Copyright (C) 2018  Yawning Angel.
// Copyright (C) 2020  Christian Loehle.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package xmppproxy

import (
	"net"

	"github.com/katzenpost/core/worker"
	"gopkg.in/op/go-logging.v1"
    "github.com/cloehle/xmppproxy/xmppserver"
    "github.com/cloehle/xmppproxy/internal/xmpp"

    "sync"
    //SMTP:
    "github.com/cloehle/xmppproxy/internal/account"
    "github.com/cloehle/xmppproxy/internal/imf"
    "github.com/cloehle/xmppproxy/event"
    "github.com/emersion/go-message"
    "time"
    "errors"
)

type xmppListener struct {
	worker.Worker
    server xmppserver.Server

	p   *Proxy
	l   net.Listener
	log *logging.Logger
}

func (l *xmppListener) Halt() {
	// Close the listener and wait for the worker(s) to return.
	l.l.Close()
	l.Worker.Halt()

	// TODO: Force close all XMPP sessions somehow.
}

func (l *xmppListener) worker() {
	addr := l.l.Addr()
	l.log.Noticef("Listening on: %v", addr)
	defer func() {
		l.log.Noticef("Stopping listening on: %v", addr)
		l.l.Close() // Usually redundant, but harmless.
	}()
	for {
		conn, err := l.l.Accept()
		if err != nil {
			if e, ok := err.(net.Error); ok && !e.Temporary() {
				l.log.Errorf("Critical accept failure: %v", err)
				return
			}
			continue
		}

		rAddr := conn.RemoteAddr()
		l.log.Debugf("Accepted new connection: %v", rAddr)
		l.Go(func() { l.server.TCPAnswer(conn) })
	}

	// NOTREACHED
}

func newXMPPListener(p *Proxy) (*xmppListener, error) {
	l := new(xmppListener)
	l.p = p
	l.log = p.logBackend.GetLogger("listener/xmpp")

	var err error
	l.l, err = net.Listen("tcp", p.cfg.Proxy.XMPPAddress)
	if err != nil {
		return nil, err
	}

	var contacts = make(map[string]chan<- []byte)
	var messagebus = make(chan xmppserver.Message)
	var connectbus = make(chan xmppserver.Connect)
	var disconnectbus = make(chan xmppserver.Disconnect)

    /*
	leader := talekcommon.NewFrontendRPC("rpc", talekconf.FrontendAddr)
	backend := libtalek.NewClient("talexmpp", *talekconf, leader)
    */
	// restore from saved contact state

	am := xmpp.AccountManager{Online: contacts, OnlineLock: &sync.Mutex{}}

	l.server = xmppserver.Server{
		Accounts:   am,
		ConnectBus: connectbus,
		Extensions: []xmppserver.Extension{
			&xmppserver.NormalMessageExtension{MessageBus: messagebus},
			&xmppserver.RosterExtension{Accounts: am},
			&GlueExtension{},
			&xmpp.RosterManagementExtension{Accounts: am},
		},
		DisconnectBus: disconnectbus,
		Domain:        "localhost",
		SkipTLS:     true,
	}

	go am.RouteRoutine(messagebus)
	go am.ConnectRoutine(connectbus)
	go am.DisconnectRoutine(disconnectbus)

	l.Go(l.worker)
	return l, nil
}

//Start smtp part

type enqueueLater struct {
	replyID      string
	accID        string
	rID          string
	payload      *[]byte
	entity       *message.Entity
	isUnreliable bool
	expire       time.Time
}

func (e *enqueueLater) sendIMFFailure(account *account.Account, err error) {
	failed := make(map[string]error)
	failed[e.rID] = err
	report, err := imf.NewEnqueueFailure(e.accID, nil, failed, e.entity.Header)
	if err == nil {
		account.StoreReport(report)
	}
}

type eventListener struct {
	worker.Worker

	p   *Proxy
	log *logging.Logger

	enqueueLaterCh chan *enqueueLater
	sendLater      map[string]*enqueueLater
}

func (l *eventListener) onKaetzchenReply(e *event.KaetzchenReplyEvent) {
	id := string(e.MessageID)
	r, ok := l.sendLater[id]
	if !ok {
		return
	}
	delete(l.sendLater, id)
	acc, _, err := l.p.getAccount(r.accID)
	if err != nil {
		l.log.Warningf("getAccount() failed for %v", r.accID)
		return
	}
	defer acc.Deref()
	rcpt, err := l.p.toAccountRecipient(r.rID)
	if err != nil {
		l.log.Warningf("toAccountRecipient() failed for %v", r.rID)
		return
	}
	if e.Err != nil {
		l.log.Warningf("KaetzchenReplyEvent received with error: %v", e.Err)
		r.sendIMFFailure(acc, e.Err)
		return
	}
	user, pubKey, err := l.p.ParseKeyQueryResponse(e.Payload)
	if err != nil {
		l.log.Warningf("ParseKeyQueryResponse returned %v", err)
		r.sendIMFFailure(acc, err)
		return
	}
	if user != rcpt.User {
		l.log.Warningf("ParseKeyQueryResponse returned WRONG USER, wanted %v got %v", rcpt.User, user)
		r.sendIMFFailure(acc, errors.New("Keyserver returned PublicKey for WRONG USER!"))
		return
	}
	l.log.Noticef("Discovered key for %v: %v", r.rID, pubKey)
	l.p.SetRecipient(r.rID, pubKey)
	report, err := imf.KeyLookupSuccess(r.accID, r.rID, pubKey)
	if err != nil {
		l.log.Warningf("Failed to produce KeyLookupSuccess report: %v", err)
		return
	}
	acc.StoreReport(report)
	rcpt, err = l.p.toAccountRecipient(r.rID)
	if err != nil {
		l.log.Warningf("Failed to lookup freshly discovered account: %v", err)
		return
	}
	_, err = acc.EnqueueMessage(rcpt, *r.payload, r.isUnreliable)
	if err != nil {
		r.sendIMFFailure(acc, err)
	}
}

func (l *eventListener) prune(t time.Time) {
	toDel := make([]string, 0)
	for k, r := range l.sendLater {
		if t.After(r.expire) {
			if acc, _, err := l.p.getAccount(r.accID); err != nil {
				toDel = append(toDel, k)
				r.sendIMFFailure(acc, errors.New("Unable to discover key for recipient"))
				acc.Deref()
			}
		}
	}
	for _, d := range toDel {
		delete(l.sendLater, d)
	}
}

func (l *eventListener) worker() {
	l.log.Debugf("Listening for events now")
	// set up state for queuing messages to send later
	l.sendLater = make(map[string]*enqueueLater)
	wakeup := func() <-chan time.Time {
		return time.After(1 * time.Minute)
	}

	for {
		select {
		case <-l.HaltCh():
			l.log.Debugf("Shutting down eventListener.")
			close(l.enqueueLaterCh)
			return
		case t := <-wakeup():
			l.log.Debugf("Waking up eventListener to prune messages")
			l.prune(t)
		case msg := <-l.enqueueLaterCh:
			l.sendLater[msg.replyID] = msg
		case evt := <-l.p.EventSink:
			switch e := evt.(type) {
			case *event.KaetzchenReplyEvent:
				l.onKaetzchenReply(e)
			default:
			}
		}
	}
}

func newEventListener(p *Proxy) *eventListener {
	l := new(eventListener)
	l.p = p
	l.log = p.logBackend.GetLogger("listener/EventSink")
	l.enqueueLaterCh = make(chan *enqueueLater)
	l.Go(l.worker)
	return l
}
/*
func newSMTPListener(p *Proxy) (*smtpListener, error) {
	l := new(smtpListener)
	l.p = p
	l.log = p.logBackend.GetLogger("listener/SMTP")

	var err error
	l.l, err = net.Listen("tcp", p.cfg.Proxy.SMTPAddress)
	if err != nil {
		return nil, err
	}

	l.Go(l.worker)
	return l, nil
}

type smtpSession struct {
	l *smtpListener

	log *logging.Logger

	nConn net.Conn
	sConn *smtpd.Conn
	id    uint64
}

func (s *smtpSession) worker() {
	defer s.nConn.Close()

	env := &smtpEnvelope{}
	defer env.Reset() // This holds an account.Account, which is refcounted.

	var viaESMTP bool
evLoop:
	for {
		ev := s.sConn.Next()
		switch ev.What {
		case smtpd.DONE, smtpd.ABORT:
			break evLoop
		case smtpd.COMMAND:
			// Check for cancelation.  This assumes the peer is going
			// to be sending commands in a timely manner, which seems
			// reasonable in the context of a local mail proxy.
			select {
			case <-s.l.HaltCh():
				s.sConn.RejectMsg("Server shutting down")
				break evLoop
			default:
			}

			// Conn.Next() will enforce command ordering, so this
			// can just accumulate based on the command, resetting
			// as appropriate.
			switch ev.Cmd {
			case smtpd.HELO:
				viaESMTP = false
				env.Reset()
			case smtpd.EHLO:
				viaESMTP = true
				env.Reset()
			case smtpd.RSET:
				env.Reset()
			case smtpd.MAILFROM:
				acc, accID, err := s.l.p.getAccount(ev.Arg)
				if err != nil {
					s.log.Warningf("Invalid MAIL FROM argument '%v': %v", ev.Arg, err)
					s.sConn.Reject()
					break
				}
				s.log.Debugf("Set account: '%v'", accID)
				env.SetAccount(accID, acc) // Takes ownership of the acc ref count.
			case smtpd.RCPTTO:
				rcpt, err := s.l.p.toAccountRecipient(ev.Arg)
				if err != nil {
					s.log.Warningf("Invalid RCPT TO argument '%v': %v", ev.Arg, err)
					s.sConn.Reject()
					break
				}
				// If automatic key discovery is enabled for this account, continue.
				if rcpt.PublicKey == nil && !env.account.InsecureKeyDiscovery {
					s.log.Warningf("RCPT TO ('%v') does not specify a known recipient.", rcpt.ID)
					s.sConn.Reject()
					break
				}
				s.log.Debugf("Added recipient: '%v'", rcpt.ID)
				env.AddRecipient(rcpt)
			case smtpd.DATA:
			default:
				s.log.Errorf("Invalid command: %v", ev.Cmd)
				s.sConn.Reject()
				break evLoop
			}
		case smtpd.GOTDATA:
			if err := s.onGotData(env, []byte(ev.Arg), viaESMTP); err != nil {
				s.log.Errorf("Failed to handle received message: %v", err)
				s.sConn.Reject()
			}
		default:
			s.log.Errorf("Invalid event: %v", ev)
			break evLoop
		}
	}

	s.log.Debugf("Connection terminated.")
}

func (s *smtpSession) onGotData(env *smtpEnvelope, b []byte, viaESMTP bool) error {
	defer env.Reset()

	// De-duplicate the recipients.
	env.DedupRecipients()
	if len(env.recipients) == 0 {
		return nil
	}

	// Validate and pre-process the outgoing message body.
	payload, entity, isUnreliable, err := s.l.p.preprocessOutgoing(b, viaESMTP)
	if err != nil {
		return err
	}

	// TODO: It is probably worth grouping all recipients of a given message
	// into a single send queue entry instead of creating a queue entry for
	// each recipient, but this is a far more simple approach, and unlike
	// traditional MTAs, xmppproxy is only going to be servicing a single
	// user with a comparatively low volume of mail anyway.
	failed := make(map[string]error)
	var enqueued []string

	for _, recipient := range env.recipients {
		if recipient.PublicKey == nil {
			msgID, err := s.l.p.QueryKeyFromProvider(env.accountID, recipient.ID)
			if err != nil {
				s.log.Warningf("Failed to query key for '%v': ", recipient.ID, err)
				failed[recipient.ID] = err
				continue
			}
			// defer this message to be sent later
			expire := time.Now().Add(time.Duration(s.l.p.cfg.Debug.UrgentQueueLifetime) * time.Second)
			s.l.p.eventListener.enqueueLaterCh <- &enqueueLater{string(msgID), env.accountID, recipient.ID, &payload, entity, isUnreliable, expire}
		} else {
			if _, err = env.account.EnqueueMessage(recipient, payload, isUnreliable); err != nil {
				s.log.Errorf("Failed to enqueue for '%v': %v", recipient, err)
				failed[recipient.ID] = err
				continue
			} else {
				enqueued = append(enqueued, recipient.ID)
			}
		}
	}

	switch len(failed) {
	case 0:
		return nil
	case len(env.recipients):
		// Technically I think I'm supposed to create a bounce message,
		// but that's silly when I can just reject the SMTP transaction.
		//
		// LMTP fully supports rejecting at send time on a per-recipient
		// basis, but we need to use SMTP, unfortunately.
		return errEnqueueAllFailed
	default:
	}

	// Generate a multipart/report indicating which recipients failed.
	report, err := imf.NewEnqueueFailure(env.accountID, enqueued, failed, entity.Header)
	if err != nil {
		return err
	}
	return env.account.StoreReport(report)
}

func (s *smtpSession) Write(p []byte) (n int, err error) {
	// This is used to adapt the smtpd package's idea of logging to our
	// leveled logging interface.

	if len(p) == 0 {
		return 0, nil
	}

	logType := p[0]
	if logType == 'r' || logType == 'w' {
		// Keep the prefix for network read/write debug logs.
		s.log.Debug(string(p))
		return len(p), nil
	}

	logMsg := string(bytes.TrimSpace(p[1:]))
	if len(logMsg) == 0 {
		return len(p), nil
	}
	switch logType {
	case '#':
		s.log.Notice(logMsg)
	case '!':
		s.log.Error(logMsg)
	default:
		// Should never happen, according to the package docs.
		s.log.Debugf("Unknown log type '%v': %v", logType, logMsg)
	}

	return len(p), nil
}

type smtpEnvelope struct {
	account    *account.Account
	recipients []*account.Recipient
	accountID  string
}

func (e *smtpEnvelope) SetAccount(id string, a *account.Account) {
	if e.account != nil {
		e.account.Deref()
	}
	e.account = a
	e.accountID = id
}

func (e *smtpEnvelope) AddRecipient(r *account.Recipient) {
	e.recipients = append(e.recipients, r)
}

func (e *smtpEnvelope) DedupRecipients() {
	newR := make([]*account.Recipient, 0, len(e.recipients))

	dedupMap := make(map[string]bool)
	for _, v := range e.recipients {
		if !dedupMap[v.ID] {
			dedupMap[v.ID] = true
			newR = append(newR, v)
		}
	}
	e.recipients = newR
}

func (e *smtpEnvelope) Reset() {
	e.SetAccount("", nil)
	e.recipients = nil
}
*/
