package noise

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	proto "github.com/gogo/protobuf/proto"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"

	ik "github.com/ChainSafe/go-libp2p-noise/ik"
	pb "github.com/ChainSafe/go-libp2p-noise/pb"
	xx "github.com/ChainSafe/go-libp2p-noise/xx"
)

const payload_string = "noise-libp2p-static-key:"

var log = logging.Logger("noise")

type secureSession struct {
	insecure net.Conn

	initiator bool
	prologue  []byte

	localKey   crypto.PrivKey
	localPeer  peer.ID
	remotePeer peer.ID

	local  peerInfo
	remote peerInfo

	xx_ns *xx.NoiseSession
	ik_ns *ik.NoiseSession

	xx_complete bool
	ik_complete bool

	noisePipesSupport   bool
	noiseStaticKeyCache map[peer.ID]([32]byte)

	noiseKeypair *Keypair

	msgBuffer []byte
	rwLock    sync.Mutex
}

type peerInfo struct {
	noiseKey  [32]byte // static noise public key
	libp2pKey crypto.PubKey
}

// newSecureSession creates a noise session that can be configured to be initialized with a static
// noise key `noisePrivateKey`, a cache of previous peer noise keys `noiseStaticKeyCache`, an
// option `noisePipesSupport` to turn on or off noise pipes
//
// With noise pipes off, we always do XX
// With noise pipes on, we first try IK, if that fails, move to XXfallback
func newSecureSession(ctx context.Context, local peer.ID, privKey crypto.PrivKey, kp *Keypair,
	insecure net.Conn, remote peer.ID, noiseStaticKeyCache map[peer.ID]([32]byte),
	noisePipesSupport bool, initiator bool) (*secureSession, error) {

	if noiseStaticKeyCache == nil {
		noiseStaticKeyCache = make(map[peer.ID]([32]byte))
	}

	if kp == nil {
		kp = GenerateKeypair()
	}

	localPeerInfo := peerInfo{
		noiseKey:  kp.public_key,
		libp2pKey: privKey.GetPublic(),
	}

	s := &secureSession{
		insecure:            insecure,
		initiator:           initiator,
		prologue:            []byte(ID),
		localKey:            privKey,
		localPeer:           local,
		remotePeer:          remote,
		local:               localPeerInfo,
		noisePipesSupport:   noisePipesSupport,
		noiseStaticKeyCache: noiseStaticKeyCache,
		msgBuffer:           []byte{},
		noiseKeypair:        kp,
	}

	err := s.runHandshake(ctx)

	return s, err
}

func (s *secureSession) NoiseStaticKeyCache() map[peer.ID]([32]byte) {
	return s.noiseStaticKeyCache
}

func (s *secureSession) NoisePublicKey() [32]byte {
	return s.noiseKeypair.public_key
}

func (s *secureSession) NoisePrivateKey() [32]byte {
	return s.noiseKeypair.private_key
}

func (s *secureSession) readLength() (int, error) {
	buf := make([]byte, 2)
	_, err := s.insecure.Read(buf)
	return int(binary.BigEndian.Uint16(buf)), err
}

func (s *secureSession) writeLength(length int) error {
	buf := make([]byte, 2)
	binary.BigEndian.PutUint16(buf, uint16(length))
	_, err := s.insecure.Write(buf)
	return err
}

func (s *secureSession) setRemotePeerInfo(key []byte) (err error) {
	s.remote.libp2pKey, err = crypto.UnmarshalPublicKey(key)
	return err
}

func (s *secureSession) setRemotePeerID(key crypto.PubKey) (err error) {
	s.remotePeer, err = peer.IDFromPublicKey(key)
	return err
}

func (s *secureSession) verifyPayload(payload *pb.NoiseHandshakePayload, noiseKey [32]byte) (err error) {
	sig := payload.GetNoiseStaticKeySignature()
	msg := append([]byte(payload_string), noiseKey[:]...)

	log.Debugf("verifyPayload msg=%x", msg)

	ok, err := s.RemotePublicKey().Verify(msg, sig)
	if err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("did not verify payload")
	}

	return nil
}

func (s *secureSession) runHandshake(ctx context.Context) error {
	// setup libp2p keys
	localKeyRaw, err := s.LocalPublicKey().Bytes()
	if err != nil {
		return fmt.Errorf("runHandshake err getting raw pubkey: %s", err)
	}

	// sign noise data for payload
	noise_pub := s.noiseKeypair.public_key
	signedPayload, err := s.localKey.Sign(append([]byte(payload_string), noise_pub[:]...))
	if err != nil {
		log.Errorf("runHandshake signing payload err=%s", err)
		return fmt.Errorf("runHandshake signing payload err=%s", err)
	}

	// create payload
	payload := new(pb.NoiseHandshakePayload)
	payload.Libp2PKey = localKeyRaw
	payload.NoiseStaticKeySignature = signedPayload
	payloadEnc, err := proto.Marshal(payload)
	if err != nil {
		log.Errorf("runHandshake marshal payload err=%s", err)
		return fmt.Errorf("runHandshake proto marshal payload err=%s", err)
	}

	// if we have the peer's noise static key (and we're the initiator,) and we support noise pipes,
	// we can try IK first. if we support noise pipes and we're not the initiator, try IK first
	// otherwise, default to XX
	if (!s.initiator && s.noiseStaticKeyCache[s.remotePeer] != [32]byte{}) && s.noisePipesSupport {
		// known static key for peer, try IK
		buf, err := s.runHandshake_ik(ctx, payloadEnc)
		if err != nil {
			log.Error("runHandshake ik err=%s", err)

			// IK failed, pipe to XXfallback
			err = s.runHandshake_xx(ctx, true, payloadEnc, buf)
			if err != nil {
				log.Error("runHandshake xx err=err", err)
				return fmt.Errorf("runHandshake xx err=%s", err)
			}

			s.xx_complete = true
		}

		s.ik_complete = true

	} else {
		// unknown static key for peer, try XX
		err := s.runHandshake_xx(ctx, false, payloadEnc, nil)
		if err != nil {
			log.Error("runHandshake xx err=%s", err)
			return err
		}

		s.xx_complete = true
	}

	return nil
}

func (s *secureSession) LocalAddr() net.Addr {
	return s.insecure.LocalAddr()
}

func (s *secureSession) LocalPeer() peer.ID {
	return s.localPeer
}

func (s *secureSession) LocalPrivateKey() crypto.PrivKey {
	return s.localKey
}

func (s *secureSession) LocalPublicKey() crypto.PubKey {
	return s.localKey.GetPublic()
}

func (s *secureSession) Read(buf []byte) (int, error) {
	l := len(buf)

	// if we have previously unread bytes, and they fit into the buf, copy them over and return
	if l <= len(s.msgBuffer) {
		copy(buf, s.msgBuffer)
		s.msgBuffer = s.msgBuffer[l:]
		return l, nil
	}

	// read length of encrypted message
	l, err := s.readLength()
	if err != nil {
		return 0, err
	}

	// read and decrypt ciphertext
	ciphertext := make([]byte, l)
	_, err = s.insecure.Read(ciphertext)
	if err != nil {
		log.Error("read ciphertext err", err)
		return 0, err
	}

	plaintext, err := s.Decrypt(ciphertext)
	if err != nil {
		log.Error("decrypt err", err)
		return 0, err
	}

	// append plaintext to message buffer, copy over what can fit in the buf
	// then advance message buffer to remove what was copied
	s.msgBuffer = append(s.msgBuffer, plaintext...)
	c := copy(buf, s.msgBuffer)
	s.msgBuffer = s.msgBuffer[c:]

	return c, nil
}

func (s *secureSession) RemoteAddr() net.Addr {
	return s.insecure.RemoteAddr()
}

func (s *secureSession) RemotePeer() peer.ID {
	return s.remotePeer
}

func (s *secureSession) RemotePublicKey() crypto.PubKey {
	return s.remote.libp2pKey
}

func (s *secureSession) SetDeadline(t time.Time) error {
	return s.insecure.SetDeadline(t)
}

func (s *secureSession) SetReadDeadline(t time.Time) error {
	return s.insecure.SetReadDeadline(t)
}

func (s *secureSession) SetWriteDeadline(t time.Time) error {
	return s.insecure.SetWriteDeadline(t)
}

func (s *secureSession) Write(in []byte) (int, error) {
	s.rwLock.Lock()
	defer s.rwLock.Unlock()

	ciphertext, err := s.Encrypt(in)
	if err != nil {
		log.Error("encrypt error", err)
		return 0, err
	}

	err = s.writeLength(len(ciphertext))
	if err != nil {
		log.Error("write length err", err)
		return 0, err
	}

	_, err = s.insecure.Write(ciphertext)
	return len(in), err
}

func (s *secureSession) Close() error {
	return s.insecure.Close()
}
