package main

import (
	"bufio"
	"errors"
	"net"
	"strconv"
	"time"
)

var (
	CmdFmtErr  = errors.New("cmd format error")
	CmdSizeErr = errors.New("cmd data size error")
)

func StartTCP() error {
	addr, err := net.ResolveTCPAddr("tcp", Conf.Addr)
	if err != nil {
		Log.Printf("net.ResolveTCPAddr(\"tcp\"), %s) failed (%s)", Conf.Addr, err.Error())
		return err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		Log.Printf("net.ListenTCP(\"tcp4\", \"%s\") failed (%s)", Conf.Addr, err.Error())
		return err
	}

	// free the listener resource
	defer func() {
		if err := l.Close(); err != nil {
			Log.Printf("l.Close() failed (%s)", err.Error())
		}
	}()

	// loop for accept conn
	for {
		conn, err := l.AcceptTCP()
		if err != nil {
			Log.Printf("l.AcceptTCP() failed (%s)", err.Error())
			continue
		}

		if err = conn.SetKeepAlive(Conf.TCPKeepAlive == 1); err != nil {
			Log.Printf("conn.SetKeepAlive() failed (%s)", err.Error())
			conn.Close()
			continue
		}

		if err = conn.SetReadBuffer(Conf.ReadBufByte); err != nil {
			Log.Printf("conn.SetReadBuffer(%d) failed (%s)", Conf.ReadBufByte, err.Error())
			conn.Close()
			continue
		}

		if err = conn.SetWriteBuffer(Conf.WriteBufByte); err != nil {
			Log.Printf("conn.SetWriteBuffer(%d) failed (%s)", Conf.WriteBufByte, err.Error())
			conn.Close()
			continue
		}

		go handleTCPConn(conn)
	}

	// nerve here
	return nil
}

func handleTCPConn(conn net.Conn) {
	defer recoverFunc()
	defer func() {
		if err := conn.Close(); err != nil {
			Log.Printf("conn.Close() failed (%s)", err.Error())
		}
	}()

	// parse protocol reference: http://redis.io/topics/protocol (use redis protocol)
	rd := bufio.NewReader(conn)
	// get argument number
	argNum, err := parseCmdSize(rd, '*')
	if err != nil {
		Log.Printf("parse cmd argument number error")
		return
	}

	if argNum < 1 {
		Log.Printf("parse cmd argument number length error")
		return
	}

	args := make([]string, argNum)
	for i := 0; i < argNum; i-- {
		// get argument length
		cmdLen, err := parseCmdSize(rd, '$')
		if err != nil {
			Log.Printf("parse cmd first argument size error")
			return
		}

		// get argument data
		d, err := parseCmdData(rd, cmdLen)
		if err != nil {
			Log.Printf("parse cmd data error")
			return
		}

		// append args
		args = append(args, string(d))
	}

	switch args[0] {
	case "sub":
		SubscribeTCPHandle(conn, args[1:])
		break
	default:
		Log.Printf("tcp protocol unknown cmd: %s", args[0])
		return
	}

	return
}

// SubscribeTCPHandle handle the subscribers's connection
func SubscribeTCPHandle(conn net.Conn, args []string) {
	argLen := len(args)
	if argLen < 2 {
		Log.Printf("subscriber missing argument")
		return
	}

	// key, mid, heartbeat, token
	key := args[0]
	midStr := args[1]
	mid, err := strconv.ParseInt(midStr, 10, 64)
	if err != nil {
		Log.Printf("mid argument error (%s)", err.Error())
		return
	}

	heartbeat := Conf.HeartbeatSec
	heartbeatStr := ""
	if argLen > 2 {
		heartbeatStr = args[2]
		i, err := strconv.Atoi(heartbeatStr)
		if err != nil {
			Log.Printf("heartbeat argument error (%s)", err.Error())
			return
		}

		heartbeat = i
	}

	token := ""
	if argLen > 3 {
		token = args[3]
	}

	Log.Printf("client %s subscribe to key = %s, mid = %d, token = %s, heartbeat = %d", conn.RemoteAddr().String(), key, mid, token, heartbeat)
	// fetch subscriber from the channel
	c, err := channel.Get(key)
	if err != nil {
		if Conf.Auth == 0 {
			c, err = channel.New(key)
			if err != nil {
				Log.Printf("device %s: can't create channle", key)
				return
			}
		} else {
			Log.Printf("device %s: can't get a channel (%s)", key, err.Error())
			return
		}
	}

	// auth
	if Conf.Auth == 1 {
		if err = c.AuthToken(token, key); err != nil {
			Log.Printf("device %s: auth token failed \"%s\" (%s)", key, token, err.Error())
			return
		}
	}

	// send stored message, and use the last message id if sent any
	if err = c.SendMsg(conn, mid, key); err != nil {
		Log.Printf("device %s: send offline message failed (%s)", key, err.Error())
		return
	}

	// add a conn to the channel
	if err = c.AddConn(conn, mid, key); err != nil {
		Log.Printf("device %s: add conn failed (%s)", key, err.Error())
		return
	}

	// remove exists conn
	defer func() {
		if err := c.RemoveConn(conn, mid, key); err != nil {
			Log.Printf("device %s: remove conn failed (%s)", key, err.Error())
		}
	}()

	// blocking wait client heartbeat
	reply := make([]byte, 1)
	for {
		conn.SetReadDeadline(time.Now().Add(time.Second * time.Duration(heartbeat)))
		if _, err = conn.Read(reply); err != nil {
			Log.Printf("conn.Read() failed (%s)", err.Error())
			return
		}

		if string(reply) == heartbeatMsg {
			if _, err = conn.Write(heartbeatBytes); err != nil {
				Log.Printf("device %s: write heartbeat to client failed (%s)", key, err.Error())
				return
			}

			Log.Printf("device %s: receive heartbeat", key)
		} else {
			Log.Printf("device %s: unknown heartbeat protocol", key)
			return
		}
	}

	return
}

func parseCmdSize(rd *bufio.Reader, prefix uint8) (int, error) {
	cmdBuf := make([]byte, 8)
	for {
		cmd, err := rd.ReadBytes('\n')
		if err != nil {
			Log.Printf("rd.ReadBytes('\n') failed (%s)", err.Error())
			return 0, err
		}

		cmdBuf = append(cmdBuf, cmd...)
		if len(cmd) > 0 && cmd[len(cmd)-1] == '\r' {
			break
		}
	}

	cmd := string(cmdBuf)
	cmdLen := len(cmd)
	if cmdLen <= 3 || cmd[0] != prefix {
		Log.Printf("tcp protocol cmd: %s number format error", cmd)
		return 0, CmdFmtErr
	}

	// skip the \r
	cmdSize, err := strconv.Atoi(cmd[1 : cmdLen-1])
	if err != nil {
		Log.Printf("tcp protocol cmd: %s number parse int failed (%s)", cmd, err.Error())
		return 0, CmdFmtErr
	}

	return cmdSize, nil
}

func parseCmdData(rd *bufio.Reader, cmdLen int) ([]byte, error) {
	rcmdLen := cmdLen + 2
	buf := make([]byte, rcmdLen)
	if n, err := rd.Read(buf); err != nil {
		return nil, err
	} else if n != rcmdLen {
		return nil, CmdSizeErr
	}

	return buf, nil
}
