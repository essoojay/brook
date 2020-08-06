// Copyright (c) 2016-present Cloud <cloud@txthinking.com>
//
// This program is free software; you can redistribute it and/or
// modify it under the terms of version 3 of the GNU General Public
// License as published by the Free Software Foundation.
//
// This program is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU
// General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package brook

import (
	"encoding/binary"
	"io"
	"log"
	"net"
	"strings"
	"time"

	cache "github.com/patrickmn/go-cache"
	"github.com/txthinking/brook/limits"
	"github.com/txthinking/brook/plugin"
	"github.com/txthinking/runnergroup"
	"github.com/txthinking/socks5"
)

// Server.
type Server struct {
	Password      []byte
	TCPAddr       *net.TCPAddr
	UDPAddr       *net.UDPAddr
	TCPListen     *net.TCPListener
	UDPConn       *net.UDPConn
	UDPExchanges  *cache.Cache
	TCPTimeout    int
	UDPTimeout    int
	ServerAuthman plugin.ServerAuthman
	RunnerGroup   *runnergroup.RunnerGroup
	UDPSrc        *cache.Cache
}

// NewServer.
func NewServer(addr, password string, tcpTimeout, udpTimeout int) (*Server, error) {
	taddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	cs := cache.New(cache.NoExpiration, cache.NoExpiration)
	cs2 := cache.New(cache.NoExpiration, cache.NoExpiration)
	if err := limits.Raise(); err != nil {
		log.Println("Try to raise system limits, got", err)
	}
	s := &Server{
		Password:     []byte(password),
		TCPAddr:      taddr,
		UDPAddr:      uaddr,
		UDPExchanges: cs,
		TCPTimeout:   tcpTimeout,
		UDPTimeout:   udpTimeout,
		RunnerGroup:  runnergroup.New(),
		UDPSrc:       cs2,
	}
	return s, nil
}

// SetServerAuthman sets authman plugin.
func (s *Server) SetServerAuthman(m plugin.ServerAuthman) {
	s.ServerAuthman = m
}

// Run server.
func (s *Server) ListenAndServe() error {
	s.RunnerGroup.Add(&runnergroup.Runner{
		Start: func() error {
			return s.RunTCPServer()
		},
		Stop: func() error {
			if s.TCPListen != nil {
				return s.TCPListen.Close()
			}
			return nil
		},
	})
	s.RunnerGroup.Add(&runnergroup.Runner{
		Start: func() error {
			return s.RunUDPServer()
		},
		Stop: func() error {
			if s.UDPConn != nil {
				return s.UDPConn.Close()
			}
			return nil
		},
	})
	return s.RunnerGroup.Wait()
}

// RunTCPServer starts tcp server.
func (s *Server) RunTCPServer() error {
	var err error
	s.TCPListen, err = net.ListenTCP("tcp", s.TCPAddr)
	if err != nil {
		return err
	}
	defer s.TCPListen.Close()
	for {
		c, err := s.TCPListen.AcceptTCP()
		if err != nil {
			return err
		}
		go func(c *net.TCPConn) {
			defer c.Close()
			if s.TCPTimeout != 0 {
				if err := c.SetDeadline(time.Now().Add(time.Duration(s.TCPTimeout) * time.Second)); err != nil {
					log.Println(err)
					return
				}
			}
			if err := s.TCPHandle(c); err != nil {
				log.Println(err)
			}
		}(c)
	}
	return nil
}

// RunUDPServer starts udp server.
func (s *Server) RunUDPServer() error {
	var err error
	s.UDPConn, err = net.ListenUDP("udp", s.UDPAddr)
	if err != nil {
		return err
	}
	defer s.UDPConn.Close()
	for {
		b := make([]byte, 65535)
		n, addr, err := s.UDPConn.ReadFromUDP(b)
		if err != nil {
			return err
		}
		go func(addr *net.UDPAddr, b []byte) {
			if err := s.UDPHandle(addr, b); err != nil {
				log.Println(err)
				return
			}
		}(addr, b[0:n])
	}
	return nil
}

// TCPHandle handles request.
func (s *Server) TCPHandle(c *net.TCPConn) error {
	cn := make([]byte, 12)
	if _, err := io.ReadFull(c, cn); err != nil {
		return err
	}
	ck, err := GetKey(s.Password, cn)
	if err != nil {
		return err
	}
	var b []byte
	b, cn, err = ReadFrom(c, ck, cn, true)
	if err != nil {
		return err
	}
	address := socks5.ToAddress(b[0], b[1:len(b)-2], b[len(b)-2:])
	a := b[0]

	var ai plugin.Internet
	if s.ServerAuthman != nil {
		b, cn, err = ReadFrom(c, ck, cn, false)
		if err != nil {
			return err
		}
		ai, err = s.ServerAuthman.VerifyToken(b, "tcp", a, address, nil)
		if err != nil {
			return err
		}
		defer ai.Close()
	}

	debug("dial tcp", address)
	tmp, err := Dial.Dial("tcp", address)
	if err != nil {
		return err
	}
	rc := tmp.(*net.TCPConn)
	defer rc.Close()
	if s.TCPTimeout != 0 {
		if err := rc.SetDeadline(time.Now().Add(time.Duration(s.TCPTimeout) * time.Second)); err != nil {
			return err
		}
	}

	go func() {
		k, n, err := PrepareKey(s.Password)
		if err != nil {
			log.Println(err)
			return
		}
		i, err := c.Write(n)
		if err != nil {
			return
		}
		if ai != nil {
			if err := ai.TCPEgress(i); err != nil {
				log.Println(err)
				return
			}
		}
		var b [1024 * 2]byte
		for {
			if s.TCPTimeout != 0 {
				if err := rc.SetDeadline(time.Now().Add(time.Duration(s.TCPTimeout) * time.Second)); err != nil {
					return
				}
			}
			i, err := rc.Read(b[:])
			if err != nil {
				return
			}
			n, i, err = WriteTo(c, b[0:i], k, n, false)
			if err != nil {
				return
			}
			if ai != nil {
				if err := ai.TCPEgress(i); err != nil {
					log.Println(err)
					return
				}
			}
		}
	}()

	for {
		if s.TCPTimeout != 0 {
			if err := c.SetDeadline(time.Now().Add(time.Duration(s.TCPTimeout) * time.Second)); err != nil {
				return nil
			}
		}
		b, cn, err = ReadFrom(c, ck, cn, false)
		if err != nil {
			return nil
		}
		i, err := rc.Write(b)
		if err != nil {
			return nil
		}
		if ai != nil {
			if err := ai.TCPEgress(i); err != nil {
				return err
			}
		}
	}
	return nil
}

type ServerUDPExchange struct {
	ClientAddr *net.UDPAddr
	RemoteConn *net.UDPConn
	Internet   plugin.Internet
}

// UDPHandle handles packet.
func (s *Server) UDPHandle(addr *net.UDPAddr, b []byte) error {
	src := addr.String()
	a, h, p, data, err := Decrypt(s.Password, b)
	if err != nil {
		return err
	}
	send := func(ue *ServerUDPExchange, data []byte) error {
		if s.ServerAuthman != nil {
			l := int(binary.BigEndian.Uint16(data[len(data)-2:]))
			data = data[0 : len(data)-l-2]
		}
		i, err := ue.RemoteConn.Write(data)
		if err != nil {
			return err
		}
		if ue.Internet != nil {
			if err := ue.Internet.UDPEgress(i); err != nil {
				return err
			}
		}
		return nil
	}

	dst := socks5.ToAddress(a, h, p)
	var ue *ServerUDPExchange
	iue, ok := s.UDPExchanges.Get(src + dst)
	if ok {
		ue = iue.(*ServerUDPExchange)
		return send(ue, data)
	}

	var ai plugin.Internet
	if s.ServerAuthman != nil {
		l := int(binary.BigEndian.Uint16(data[len(data)-2:]))
		ai, err = s.ServerAuthman.VerifyToken(data[len(data)-l-2:len(data)-2], "udp", a, dst, data[0:len(data)-l-2])
		if err != nil {
			return err
		}
	}
	debug("dial udp", dst)
	var laddr *net.UDPAddr
	any, ok := s.UDPSrc.Get(src + dst)
	if ok {
		laddr = any.(*net.UDPAddr)
	}
	raddr, err := net.ResolveUDPAddr("udp", dst)
	if err != nil {
		return err
	}
	rc, err := Dial.DialUDP("udp", laddr, raddr)
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			// we dont choose lock, so ignore this error
			return nil
		}
		return err
	}
	if laddr == nil {
		s.UDPSrc.Set(src+dst, rc.LocalAddr().(*net.UDPAddr), -1)
	}
	ue = &ServerUDPExchange{
		ClientAddr: addr,
		RemoteConn: rc,
		Internet:   ai,
	}
	if err := send(ue, data); err != nil {
		ue.RemoteConn.Close()
		if ue.Internet != nil {
			ue.Internet.Close()
		}
		return err
	}
	s.UDPExchanges.Set(src+dst, ue, cache.DefaultExpiration)
	go func(ue *ServerUDPExchange, dst string) {
		defer func() {
			ue.RemoteConn.Close()
			s.UDPExchanges.Delete(ue.ClientAddr.String() + dst)
			if ue.Internet != nil {
				ue.Internet.Close()
			}
		}()
		var b [65535]byte
		for {
			if s.UDPTimeout != 0 {
				if err := ue.RemoteConn.SetDeadline(time.Now().Add(time.Duration(s.UDPTimeout) * time.Second)); err != nil {
					break
				}
			}
			n, err := ue.RemoteConn.Read(b[:])
			if err != nil {
				break
			}
			a, addr, port, err := socks5.ParseAddress(dst)
			if err != nil {
				log.Println(err)
				break
			}
			d := make([]byte, 0, 7)
			d = append(d, a)
			d = append(d, addr...)
			d = append(d, port...)
			d = append(d, b[0:n]...)
			cd, err := Encrypt(s.Password, d)
			if err != nil {
				log.Println(err)
				break
			}
			i, err := s.UDPConn.WriteToUDP(cd, ue.ClientAddr)
			if err != nil {
				break
			}
			if ue.Internet != nil {
				if err := ue.Internet.UDPEgress(i); err != nil {
					log.Println(err)
					break
				}
			}
		}
	}(ue, dst)
	return nil
}

// Shutdown server.
func (s *Server) Shutdown() error {
	return s.RunnerGroup.Done()
}
