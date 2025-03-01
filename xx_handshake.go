package noise

import (
	"context"
	"fmt"

	proto "github.com/gogo/protobuf/proto"
	"github.com/libp2p/go-libp2p-core/peer"

	pb "github.com/ChainSafe/go-libp2p-noise/pb"
	xx "github.com/ChainSafe/go-libp2p-noise/xx"
)

func (s *secureSession) xx_sendHandshakeMessage(payload []byte, initial_stage bool) error {
	var msgbuf xx.MessageBuffer
	s.xx_ns, msgbuf = xx.SendMessage(s.xx_ns, payload, nil)
	var encMsgBuf []byte
	if initial_stage {
		encMsgBuf = msgbuf.Encode0()
	} else {
		encMsgBuf = msgbuf.Encode1()
	}

	err := s.writeLength(len(encMsgBuf))
	if err != nil {
		log.Error("xx_sendHandshakeMessage initiator=%v err=%s", s.initiator, err)
		return fmt.Errorf("xx_sendHandshakeMessage write length err=%s", err)
	}

	_, err = s.insecure.Write(encMsgBuf)
	if err != nil {
		log.Error("xx_sendHandshakeMessage initiator=%v err=%s", s.initiator, err)
		return fmt.Errorf("xx_sendHandshakeMessage write to conn err=%s", err)
	}

	log.Debugf("xx_sendHandshakeMessage initiator=%v msgbuf=%v initial_stage=%v", s.initiator, msgbuf, initial_stage)

	return nil
}

func (s *secureSession) xx_recvHandshakeMessage(initial_stage bool) (buf []byte, plaintext []byte, valid bool, err error) {
	l, err := s.readLength()
	if err != nil {
		return nil, nil, false, fmt.Errorf("xx_recvHandshakeMessage read length err=%s", err)
	}

	buf = make([]byte, l)

	_, err = s.insecure.Read(buf)
	if err != nil {
		return buf, nil, false, fmt.Errorf("xx_recvHandshakeMessage read from conn err=%s", err)
	}

	var msgbuf *xx.MessageBuffer
	if initial_stage {
		msgbuf, err = xx.Decode0(buf)
	} else {
		msgbuf, err = xx.Decode1(buf)
	}

	if err != nil {
		log.Debugf("xx_recvHandshakeMessage initiator=%v decode err=%s", s.initiator, err)
		return buf, nil, false, fmt.Errorf("xx_recvHandshakeMessage decode msg err=%s", err)
	}

	s.xx_ns, plaintext, valid = xx.RecvMessage(s.xx_ns, msgbuf)
	if !valid {
		log.Error("xx_recvHandshakeMessage initiator=%v err=validation fail", s.initiator)
		return buf, nil, false, fmt.Errorf("xx_recvHandshakeMessage validation fail")
	}

	log.Debugf("xx_recvHandshakeMessage initiator=%v msgbuf=%v initial_stage=%v", s.initiator, msgbuf, initial_stage)

	return buf, plaintext, valid, nil
}

// Runs the XX handshake
// XX:
//   -> e
//   <- e, ee, s, es
//   -> s, se
// if fallback = true, initialMsg is used as the message in stage 1 of the initiator and stage 0
// of the responder
func (s *secureSession) runHandshake_xx(ctx context.Context, fallback bool, payload []byte, initialMsg []byte) (err error) {
	kp := xx.NewKeypair(s.noiseKeypair.public_key, s.noiseKeypair.private_key)

	log.Debugf("runHandshake_xx initiator=%v fallback=%v pubkey=%x", s.initiator, fallback, kp.PubKey())

	// new XX noise session
	s.xx_ns = xx.InitSession(s.initiator, s.prologue, kp, [32]byte{})

	if s.initiator {
		// stage 0 //

		if !fallback {
			err = s.xx_sendHandshakeMessage(nil, true)
			if err != nil {
				return fmt.Errorf("runHandshake_xx stage 0 initiator fail: %s", err)
			}
		} else {
			e_ik := s.ik_ns.Ephemeral()
			log.Debugf("runHandshake_xx stage=0 initiator=true fallback=true ephemeralkeys=%x", e_ik)
			e_xx := xx.NewKeypair(e_ik.PubKey(), e_ik.PrivKey())

			// initialize state as if we sent the first message
			var msgbuf xx.MessageBuffer
			s.xx_ns, msgbuf = xx.SendMessage(s.xx_ns, nil, &e_xx)

			log.Debugf("runHandshake_xx stage=0 initiator=true fallback=true msgbuf=%v", msgbuf)
		}

		// stage 1 //

		var plaintext []byte
		var valid bool
		if !fallback {
			// read reply
			_, plaintext, valid, err = s.xx_recvHandshakeMessage(false)
			if err != nil {
				return fmt.Errorf("runHandshake_xx initiator stage 1 fail: %s", err)
			}

			if !valid {
				return fmt.Errorf("runHandshake_xx stage 1 initiator validation fail")
			}
		} else {
			var msgbuf *xx.MessageBuffer
			msgbuf, err = xx.Decode1(initialMsg)

			log.Debugf("xx_recvHandshakeMessage stage=1 initiator=%v msgbuf=%v", s.initiator, msgbuf)

			if err != nil {
				log.Debugf("xx_recvHandshakeMessage stage=1 initiator=%v decode_err=%s", s.initiator, err)
				return fmt.Errorf("runHandshake_xx decode msg fail: %s", err)
			}

			s.xx_ns, plaintext, valid = xx.RecvMessage(s.xx_ns, msgbuf)
			if !valid {
				log.Errorf("xx_recvHandshakeMessage initiator=%v", s.initiator, "error", "validation fail")
				return fmt.Errorf("runHandshake_xx validation fail")
			}

		}

		// stage 2 //

		err = s.xx_sendHandshakeMessage(payload, false)
		if err != nil {
			return fmt.Errorf("runHandshake_xx stage=2 initiator=true err=%s", err)
		}

		// unmarshal payload
		nhp := new(pb.NoiseHandshakePayload)
		err = proto.Unmarshal(plaintext, nhp)
		if err != nil {
			return fmt.Errorf("runHandshake_xx stage=2 initiator=true err=cannot unmarshal payload")
		}

		// set remote libp2p public key
		err = s.setRemotePeerInfo(nhp.GetLibp2PKey())
		if err != nil {
			log.Errorf("runHandshake_xx stage=2 initiator=true set remote peer info err=%s", err)
			return fmt.Errorf("runHandshake_xx stage=2 initiator=true read remote libp2p key fail")
		}

		// assert that remote peer ID matches libp2p public key
		pid, err := peer.IDFromPublicKey(s.RemotePublicKey())
		if pid != s.remotePeer {
			log.Errorf("runHandshake_xx stage=2 initiator=true check remote peer id err: expected %x got %x", s.remotePeer, pid)
		} else if err != nil {
			log.Errorf("runHandshake_xx stage 2 initiator check remote peer id err %s", err)
		}

		// verify payload is signed by libp2p key
		err = s.verifyPayload(nhp, s.xx_ns.RemoteKey())
		if err != nil {
			log.Errorf("runHandshake_xx stage=2 initiator=true verify payload err=%s", err)
		}

		if s.noisePipesSupport {
			s.noiseStaticKeyCache[s.remotePeer] = s.xx_ns.RemoteKey()
		}

	} else {

		// stage 0 //

		var plaintext []byte
		var valid bool
		nhp := new(pb.NoiseHandshakePayload)

		if !fallback {
			// read message
			_, plaintext, valid, err = s.xx_recvHandshakeMessage(true)
			if err != nil {
				return fmt.Errorf("runHandshake_xx stage=0 initiator=false err=%s", err)
			}

			if !valid {
				return fmt.Errorf("runHandshake_xx stage=0 initiator=false err=validation fail")
			}

		} else {
			var msgbuf *xx.MessageBuffer
			msgbuf, err = xx.Decode1(initialMsg)
			if err != nil {
				log.Errorf("runHandshake_xx recv msg err", err)
				return err
			}

			xx_msgbuf := xx.NewMessageBuffer(msgbuf.NE(), nil, nil)
			log.Debugf("runHandshake_xx initiator=false msgbuf=%v modified_msgbuf=%v", msgbuf, xx_msgbuf)

			s.xx_ns, plaintext, valid = xx.RecvMessage(s.xx_ns, &xx_msgbuf)
			if !valid {
				log.Errorf("runHandshake_xx initiator=false recv msg err=%s", "validation fail")
				return fmt.Errorf("runHandshake_xx validation fail")
			}
		}

		// stage 1 //

		err = s.xx_sendHandshakeMessage(payload, false)
		if err != nil {
			return fmt.Errorf("runHandshake_xx stage=1 initiator=false err=%s", err)
		}

		// stage 2 //

		// read message
		_, plaintext, valid, err = s.xx_recvHandshakeMessage(false)
		if err != nil {
			return fmt.Errorf("runHandshake_xx stage=2 initiator=false err=%s", err)
		}

		if !valid {
			return fmt.Errorf("runHandshake_xx stage=2 initiator=false err=validation fail")
		}

		log.Debugf("runHandshake_xx stage=2 initiator=false remote key=%x", s.xx_ns.RemoteKey())

		// unmarshal payload
		err = proto.Unmarshal(plaintext, nhp)
		if err != nil {
			return fmt.Errorf("runHandshake_xx stage=2 initiator=false err=cannot unmarshal payload")
		}

		// set remote libp2p public key
		err = s.setRemotePeerInfo(nhp.GetLibp2PKey())
		if err != nil {
			log.Errorf("runHandshake_xx stage=2 initiator=false set remote peer info err=%s", err)
			return fmt.Errorf("runHandshake_xx stage=2 initiator=false read remote libp2p key fail")
		}

		// assert that remote peer ID matches libp2p key
		err = s.setRemotePeerID(s.RemotePublicKey())
		if err != nil {
			log.Errorf("runHandshake_xx stage=2 initiator=false set remote peer id err=%s", err)
		}

		s.remote.noiseKey = s.xx_ns.RemoteKey()

		// verify payload is signed by libp2p key
		err = s.verifyPayload(nhp, s.remote.noiseKey)
		if err != nil {
			log.Errorf("runHandshake_xx stage=2 initiator=false verify payload err=%s", err)
			return fmt.Errorf("runHandshake_xx stage=2 initiator=false err=%s", err)
		}

		if s.noisePipesSupport {
			s.noiseStaticKeyCache[s.remotePeer] = s.remote.noiseKey
		}
	}

	log.Debugf("runHandshake_xx done initiator=%v", s.initiator)
	return nil
}
