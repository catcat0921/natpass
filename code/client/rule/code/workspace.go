package code

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lwch/logging"
	"github.com/lwch/natpass/code/client/conn"
	"github.com/lwch/natpass/code/network"
	"github.com/lwch/natpass/code/utils"
)

var errWaitingTimeout = errors.New("waiting for code-server startup more than 1 minute")

// Workspace workspace of code-server
type Workspace struct {
	sync.RWMutex
	parent *Code
	id     string
	target string
	name   string
	exec   *exec.Cmd
	cli    *http.Client
	remote *conn.Conn
	// runtime
	sendBytes  uint64
	recvBytes  uint64
	sendPacket uint64
	recvPacket uint64
	requestID  uint64
	onListen   chan struct{}
	onMessage  map[uint64]chan *network.Msg
}

func newWorkspace(parent *Code, id, name, target string, remote *conn.Conn) *Workspace {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	return &Workspace{
		parent:    parent,
		id:        id,
		target:    target,
		name:      name,
		remote:    remote,
		onListen:  make(chan struct{}),
		onMessage: make(map[uint64]chan *network.Msg),
	}
}

// GetID get link id
func (ws *Workspace) GetID() string {
	return ws.id
}

// GetBytes get send and recv bytes
func (ws *Workspace) GetBytes() (uint64, uint64) {
	return ws.recvBytes, ws.sendBytes
}

// GetPackets get send and recv packets
func (ws *Workspace) GetPackets() (uint64, uint64) {
	return ws.recvPacket, ws.sendPacket
}

// Exec execute code-server
func (ws *Workspace) Exec(dir string) error {
	workdir := filepath.Join(dir, ws.name)
	err := os.MkdirAll(workdir, 0755)
	if err != nil {
		logging.Error("can not create work dir[%s]: %v", workdir, err)
		return err
	}
	ws.exec = exec.Command("code-server", "--disable-update-check",
		"--auth", "none",
		"--socket", filepath.Join(workdir, ws.id+".sock"),
		"--user-data-dir", filepath.Join(workdir, "data"),
		"--extensions-dir", filepath.Join(workdir, "extensions"))
	stdout, err := ws.exec.StdoutPipe()
	if err != nil {
		logging.Error("can not get stdout pipe for link [%s] name [%s]", ws.id, ws.name)
		return err
	}
	stderr, err := ws.exec.StderrPipe()
	if err != nil {
		logging.Error("can not get stderr pipe for link [%s] name [%s]", ws.id, ws.name)
		return err
	}
	err = ws.exec.Start()
	if err != nil {
		logging.Error("can not start code-server for link [%s] name [%s]", ws.id, ws.name)
		return err
	}
	go func() {
		err = ws.exec.Wait()
		if err != nil {
			logging.Error("code-server [%s] [%s] exited: %v", ws.id, ws.name, err)
			return
		}
		logging.Info("code-server for link [%s] name [%s] exited", ws.id, ws.name)
	}()
	ws.cli = &http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return net.Dial("unix", filepath.Join(workdir, ws.id+".sock"))
			},
		},
	}
	go ws.log(stdout, stderr)
	select {
	case <-time.After(time.Minute):
		return errWaitingTimeout
	case <-ws.onListen:
		return nil
	}
}

// Close close workspace
func (ws *Workspace) Close() {
	if ws.exec != nil && ws.exec.Process != nil {
		ws.exec.Process.Kill()
	}
	ws.remote.SendDisconnect(ws.target, ws.id)
}

func (ws *Workspace) log(stdout, stderr io.ReadCloser) {
	defer stdout.Close()
	defer stderr.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	watch := func(target io.Reader) {
		defer wg.Done()
		s := bufio.NewScanner(target)
		for s.Scan() {
			if strings.Contains(s.Text(), "listening on") &&
				strings.Contains(s.Text(), ws.id+".sock") {
				ws.onListen <- struct{}{}
			}
			logging.Info("code-server [%s] [%s]: %s", ws.id, ws.name, s.Text())
		}
	}

	go watch(stdout)
	go watch(stderr)
	wg.Wait()
}

func (ws *Workspace) Forward() {
	go ws.remoteRead()
}

func (ws *Workspace) remoteRead() {
	defer utils.Recover("remoteRead")
	defer ws.Close()
	ch := ws.remote.ChanRead(ws.id)
	for {
		msg := <-ch
		if msg == nil {
			return
		}
		switch msg.GetXType() {
		case network.Msg_code_request:
			go ws.handleRequest(msg)
		}
	}
}

func (ws *Workspace) closeMessage(reqID uint64) {
	ws.Lock()
	if ch, ok := ws.onMessage[reqID]; ok {
		close(ch)
		delete(ws.onMessage, reqID)
	}
	ws.Unlock()
}

func (ws *Workspace) localRead() {
	defer utils.Recover("localRead")
	defer ws.Close()
	ch := ws.remote.ChanRead(ws.id)
	for {
		msg := <-ch
		if msg == nil {
			return
		}
		switch msg.GetXType() {
		case network.Msg_code_response_hdr:
			ws.writeMessage(msg.GetCsrepHdr().GetRequestId(), msg)
		case network.Msg_code_response_body:
			ws.writeMessage(msg.GetCsrepBody().GetRequestId(), msg)
		}
	}
}

func (ws *Workspace) writeMessage(reqID uint64, msg *network.Msg) {
	defer utils.Recover("writeMessage")
	ws.RLock()
	ch := ws.onMessage[reqID]
	ws.RUnlock()
	if ch != nil {
		select {
		case ch <- msg:
		case <-time.After(ws.parent.writeTimeout):
		}
	}
}

func (ws *Workspace) onResponse(reqID uint64) *network.Msg {
	ws.RLock()
	ch := ws.onMessage[reqID]
	ws.RUnlock()
	if ch != nil {
		select {
		case msg := <-ch:
			return msg
		case <-time.After(ws.parent.readTimeout):
			return nil
		}
	}
	return nil
}
