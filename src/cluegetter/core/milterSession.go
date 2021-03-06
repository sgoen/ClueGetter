// ClueGetter - Does things with mail
//
// Copyright 2016 Dolf Schimmel, Freeaqingme.
//
// This Source Code Form is subject to the terms of the Apache License, Version 2.0.
// For its contents, please refer to the LICENSE file.
//
package core

import (
	"bytes"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Freeaqingme/golang-ring"
	"github.com/golang/protobuf/proto"
)

type MilterSession struct {
	id             [16]byte
	DateConnect    time.Time
	DateDisconnect time.Time
	Messages       []*Message
	Instance       uint

	SaslUsername  string
	SaslSender    string
	SaslMethod    string
	CertIssuer    string
	CertSubject   string
	CipherBits    uint32
	Cipher        string
	TlsVersion    string
	Ip            string
	ReverseDns    string
	Hostname      string
	Helo          string
	MtaHostName   string
	MtaDaemonName string

	// Use GetIpInfo() to retrieve. This should not be
	// exported, but need to fix json encoding first.
	IpInfo   *IpInfo
	ipInfoMu *sync.Mutex

	config    *SessionConfig
	milterCtx uintptr
}

type IpInfo struct {
	ASN            string
	IpRange        string
	Country        string
	Continent      string
	ISP            string
	AllocationDate *time.Time
	Location       struct {
		Lat float64
		Lon float64
	}
}

func (m *MilterSession) Config() *SessionConfig {
	return m.config
}

func (m *MilterSession) ClientIsMonitorHost() bool {
	for _, monHost := range Config.ClueGetter.Monitor_Host {
		if strings.EqualFold(m.Ip, monHost) {
			return true
		}
	}

	return false
}

func (m *MilterSession) GetIpInfo() *IpInfo {
	m.ipInfoMu.Lock()
	defer m.ipInfoMu.Unlock()

	return m.IpInfo
}

func (m *MilterSession) SetIpInfo(ipinfo *IpInfo) {
	m.ipInfoMu.Lock()
	defer m.ipInfoMu.Unlock()

	m.IpInfo = ipinfo
}

type milterSessionWhitelistRange struct {
	ipStart net.IP
	ipEnd   net.IP
	mask    int
}

type milterSessionCluegetterClient struct {
	id         uint64
	hostname   string
	daemonName string
}

type milterSessionCluegetterClients struct {
	sync.Mutex
	clients []*milterSessionCluegetterClient
}

var milterSessionInsertStmt = *new(*sql.Stmt)
var milterCluegetterClientInsertStmt = *new(*sql.Stmt)
var milterSessionWhitelist []*milterSessionWhitelistRange
var milterSessionClients milterSessionCluegetterClients

var milterSessionPersistChan = make(chan []byte, 100)
var milterSessionPersistQueue ring.Ring

func milterSessionPrepStmt() {
	stmt, err := Rdbms.Prepare(`
		INSERT INTO session(id, cluegetter_instance, cluegetter_client, date_connect,
							date_disconnect, ip, reverse_dns, helo, sasl_username,
							sasl_method, cert_issuer, cert_subject, cipher_bits, cipher,
							tls_version)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE date_disconnect=?
	`)
	if err != nil {
		Log.Fatalf("%s", err)
	}

	milterSessionInsertStmt = stmt

	stmt, err = Rdbms.Prepare(`
		INSERT INTO cluegetter_client (hostname, daemon_name) VALUES(?,?)
			ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id)`)
	if err != nil {
		Log.Fatalf("%s", err)
	}

	milterCluegetterClientInsertStmt = stmt
}

func (s *MilterSession) getNewMessage() *Message {
	msg := NewMessage()
	msg.SetSession(s)

	s.Messages = append(s.Messages, msg)
	return msg
}

func (s *MilterSession) getLastMessage() *Message {
	return s.Messages[len(s.Messages)-1]
}

func (s *MilterSession) Id() []byte {
	return s.id[:]
}

func (s *MilterSession) IdArray() [16]byte {
	return s.id
}

func (s *MilterSession) getId() [16]byte {
	return s.id
}

func (s *MilterSession) SetId(id [16]byte) {
	if s.IdArray() != [16]byte{} {
		panic("Cannot set a session id once set")
	}

	if id == [16]byte{} {
		panic("Cannot set zero-valued id")
	}
	s.id = id
}

func (s *MilterSession) getSaslUsername() string {
	return s.SaslUsername
}

func (s *MilterSession) getSaslSender() string {
	return s.SaslSender
}

func (s *MilterSession) getSaslMethod() string {
	return s.SaslMethod
}

func (s *MilterSession) getCertIssuer() string {
	return s.CertIssuer
}

func (s *MilterSession) getCertSubject() string {
	return s.CertSubject
}

func (s *MilterSession) getCipherBits() uint32 {
	return s.CipherBits
}

func (s *MilterSession) getCipher() string {
	return s.Cipher
}

func (s *MilterSession) getTlsVersion() string {
	return s.TlsVersion
}

func (s *MilterSession) getIp() string {
	return s.Ip
}

func (s *MilterSession) getReverseDns() string {
	return s.ReverseDns
}

func (s *MilterSession) getHostname() string {
	return s.Hostname
}

func (s *MilterSession) getHelo() string {
	return s.Helo
}

func (s *MilterSession) getMtaHostName() string {
	return s.MtaHostName
}

func (s *MilterSession) getMtaDaemonName() string {
	return s.MtaDaemonName
}

func (s *MilterSession) isWhitelisted() bool {
	testIP := net.ParseIP(s.getIp()).To16()
	for _, whitelistRange := range milterSessionWhitelist {
		if bytes.Compare(testIP, whitelistRange.ipStart) >= 0 &&
			bytes.Compare(testIP, whitelistRange.ipEnd) <= 0 {
			return true
		}
	}

	return false
}

func milterSessionStart() {
	milterSessionPrepStmt()

	milterSessionWhitelist = make([]*milterSessionWhitelistRange, len(Config.ClueGetter.Whitelist))
	for idx, ipString := range Config.ClueGetter.Whitelist {
		if !strings.Contains(ipString, "/") {
			if strings.Contains(ipString, ":") {
				ipString = ipString + "/128"
			} else {
				ipString = ipString + "/32"
			}
		}
		_, ip, err := net.ParseCIDR(ipString)
		if ip == nil || err != nil {
			panic(fmt.Sprintf("Invalid whitelist ip specified '%s': %s", ipString, err))
		}

		ipEnd := make([]byte, len(ip.IP))
		for k, v := range ip.IP {
			ipEnd[k] = v | (ip.Mask[k] ^ 0xff)
		}

		mask, _ := strconv.Atoi(ipString[strings.Index(ipString, "/")+1:])
		milterSessionWhitelist[idx] = &milterSessionWhitelistRange{ip.IP.To16(), net.IP(ipEnd).To16(), mask}
	}

	messagePersistQueue = make(chan []byte)
	if Config.ClueGetter.Rdbms_Message_Persist {
		in := make(chan []byte)
		redisListSubscribe("cluegetter-" + strconv.Itoa(int(instance)) + "-session-persist", milterSessionPersistChan, in)
		go milterSessionPersistHandleQueue(in)
	}

	milterSessionPersistQueue = ring.Ring{}
	milterSessionPersistQueue.SetCapacity(256)

	go func() {
		ticker := time.NewTicker(60 * time.Second)
		for {
			select {
			case <-ticker.C:
				Log.Infof("milterSessionPersistQueue has %d items",
					milterSessionPersistQueue.ContentSize(),
				)
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		for {
			select {
			case <-ticker.C:
				milterSessionProcessQueue()
			}
		}
	}()

	Log.Infof("Milter Session module started successfully")
}

func milterSessionProcessQueue() {
	for {
		queueItem := milterSessionPersistQueue.Dequeue()
		if queueItem == nil {
			break
		}

		milterSessionPostDisconnectModule(queueItem.(*MilterSession))
	}

}

func milterSessionPostDisconnectModule(s *MilterSession) {
	for _, module := range cg.Modules() {
		go func(m Module, s *MilterSession) {
			CluegetterRecover(m.Name() + ".SessionDisconnect")
			m.SessionDisconnect(s)
		}(module, s)
	}
}

func milterSessionConfigureModule(s *MilterSession) {
	for _, module := range cg.Modules() {
		// Modules are expected to modify the SessionConfig
		// So we do not run them concurrently
		module.SessionConfigure(s)
	}
}

func milterSessionPersistHandleQueue(queue chan []byte) {
	for {
		data := <-queue
		go milterSessionPersistProtoBuf(data)
	}
}

func milterSessionPersistProtoBuf(protoBuf []byte) {
	defer func() {
		if Config.ClueGetter.Exit_On_Panic {
			return
		}
		r := recover()
		if r == nil {
			return
		}
		Log.Errorf("Panic caught in milterSessionPersistProtoBuf(). Recovering. Error: %s", r)
		return
	}()

	msg := &Proto_Session{}
	err := proto.Unmarshal(protoBuf, msg)
	if err != nil {
		panic("unmarshaling error: " + err.Error())
	}

	milterSessionPersist(msg)
	return
}

func milterSessionPersist(sess *Proto_Session) {
	client := milterSessionGetClient(sess.MtaHostName, sess.MtaDaemonName)

	var date_disconnect time.Time
	if sess.TimeEnd != 0 {
		date_disconnect = time.Unix(int64(sess.TimeEnd), 0)
	}

	StatsCounters["RdbmsQueries"].increase(1)
	_, err := milterSessionInsertStmt.Exec(
		string(sess.Id[:]), sess.InstanceId, client.id, time.Unix(int64(sess.TimeStart), 0),
		date_disconnect, sess.Ip, sess.ReverseDns, sess.Helo, sess.SaslUsername,
		sess.SaslMethod, sess.CertIssuer, sess.CertSubject, sess.CipherBits, sess.Cipher,
		sess.TlsVersion, date_disconnect,
	)
	if err != nil {
		panic("Could not execute milterSessionInsertStmt in milterSession.persist(): " + err.Error())
	}
}

func (s *MilterSession) persist() {
	// Whether or not the client is a monitor host we still enqueue the session.
	// It's used for the SessionDisconnect() callback which could be used for other
	// purposes than persisting as well.
	milterSessionPersistQueue.Enqueue(s) // TODO: Log if ring buffer is (near) full

	if s.ClientIsMonitorHost() && len(s.Messages) == 0 {
		return
	}

	protoMsg, err := proto.Marshal(s.getProtoBufStruct())
	if err != nil {
		panic("marshaling error: " + err.Error())
	}


	if Config.ClueGetter.Rdbms_Message_Persist {
		milterSessionPersistChan <- protoMsg
	}
}

func (sess *MilterSession) getProtoBufStruct() *Proto_Session {
	timeStart := sess.DateConnect.Unix()
	var timeEnd int64
	if &sess.DateDisconnect != nil {
		timeEnd = sess.DateDisconnect.Unix()
	}
	instanceId := uint64(instance)
	return &Proto_Session{
		InstanceId:    instanceId,
		Id:            sess.id[:],
		TimeStart:     uint64(timeStart),
		TimeEnd:       uint64(timeEnd),
		SaslUsername:  sess.SaslUsername,
		SaslSender:    sess.SaslSender,
		SaslMethod:    sess.SaslMethod,
		CertIssuer:    sess.CertIssuer,
		CertSubject:   sess.CertSubject,
		CipherBits:    sess.CipherBits,
		Cipher:        sess.Cipher,
		TlsVersion:    sess.TlsVersion,
		Ip:            sess.Ip,
		ReverseDns:    sess.ReverseDns,
		Hostname:      sess.Hostname,
		Helo:          sess.Helo,
		MtaHostName:   sess.MtaHostName,
		MtaDaemonName: sess.MtaDaemonName,
	}
}

func milterSessionGetClient(hostname string, daemonName string) *milterSessionCluegetterClient {
	milterSessionClients.Lock()
	defer milterSessionClients.Unlock()

	for _, client := range milterSessionClients.clients {
		if client.hostname == hostname && client.daemonName == daemonName {
			return client
		}
	}

	res, err := milterCluegetterClientInsertStmt.Exec(hostname, daemonName)
	if err != nil {
		panic("Could not insert new Cluegetter Client: " + err.Error())
	}

	id, err := res.LastInsertId()
	if err != nil {
		panic("Could not get lastinsertid from milterCluegetterClientInsertStmt: " + err.Error())
	}

	client := &milterSessionCluegetterClient{uint64(id), hostname, daemonName}
	milterSessionClients.clients = append(milterSessionClients.clients, client)
	return client
}
