package gateway

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/brocaar/loraserver"
	"github.com/brocaar/lorawan"

	log "github.com/Sirupsen/logrus"
)

var errGatewayDoesNotExist = errors.New("gateway does not exist")
var gatewayCleanupDuration = -1 * time.Minute

type udpPacket struct {
	addr *net.UDPAddr
	data []byte
}

type gateway struct {
	addr     *net.UDPAddr
	lastSeen time.Time
}

type gateways struct {
	sync.RWMutex
	gateways map[lorawan.EUI64]gateway
	onNew    func(lorawan.EUI64) error
	onDelete func(lorawan.EUI64) error
}

func (c *gateways) get(mac lorawan.EUI64) (gateway, error) {
	defer c.RUnlock()
	c.RLock()
	gw, ok := c.gateways[mac]
	if !ok {
		return gw, errGatewayDoesNotExist
	}
	return gw, nil
}

func (c *gateways) set(mac lorawan.EUI64, gw gateway) error {
	defer c.Unlock()
	c.Lock()
	_, ok := c.gateways[mac]
	if !ok && c.onNew != nil {
		if err := c.onNew(mac); err != nil {
			return err
		}
	}
	c.gateways[mac] = gw
	return nil
}

func (c *gateways) cleanup() error {
	defer c.Unlock()
	c.Lock()
	for mac := range c.gateways {
		if c.gateways[mac].lastSeen.Before(time.Now().Add(gatewayCleanupDuration)) {
			if c.onDelete != nil {
				if err := c.onDelete(mac); err != nil {
					return err
				}
			}
			delete(c.gateways, mac)
		}
	}
	return nil
}

// Backend implements a Semtech gateway backend.
type Backend struct {
	conn        *net.UDPConn
	rxChan      chan loraserver.RXPacket
	statsChan   chan loraserver.GatewayStatsPacket
	udpSendChan chan udpPacket
	closed      bool
	gateways    gateways
	wg          sync.WaitGroup
}

// NewBackend creates a new backend.
func NewBackend(bind string, onNew func(lorawan.EUI64) error, onDelete func(lorawan.EUI64) error) (*Backend, error) {
	addr, err := net.ResolveUDPAddr("udp", bind)
	if err != nil {
		return nil, err
	}
	log.WithField("addr", addr).Info("starting gateway udp listener")
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}

	b := &Backend{
		conn:        conn,
		rxChan:      make(chan loraserver.RXPacket),
		statsChan:   make(chan loraserver.GatewayStatsPacket),
		udpSendChan: make(chan udpPacket),
		gateways: gateways{
			gateways: make(map[lorawan.EUI64]gateway),
			onNew:    onNew,
			onDelete: onDelete,
		},
	}

	go func() {
		for {
			if err := b.gateways.cleanup(); err != nil {
				log.Errorf("backend/mqttpubsub: gateways cleanup failed: %s", err)
			}
			time.Sleep(time.Minute)
		}
	}()

	go func() {
		b.wg.Add(1)
		err := b.readPackets()
		if !b.closed {
			log.Fatal(err)
		}
		b.wg.Done()
	}()

	go func() {
		b.wg.Add(1)
		err := b.sendPackets()
		if !b.closed {
			log.Fatal(err)
		}
		b.wg.Done()
	}()

	return b, nil
}

// Close closes the backend.
func (b *Backend) Close() error {
	b.closed = true
	close(b.udpSendChan)
	if err := b.conn.Close(); err != nil {
		return err
	}
	b.wg.Wait()
	return nil
}

// RXPacketChan returns the channel containing the received RX packets.
func (b *Backend) RXPacketChan() chan loraserver.RXPacket {
	return b.rxChan
}

// StatsChan returns the channel containg the received gateway stats.
func (b *Backend) StatsChan() chan loraserver.GatewayStatsPacket {
	return b.statsChan
}

// Send sends the given packet to the gateway.
func (b *Backend) Send(txPacket loraserver.TXPacket) error {
	gw, err := b.gateways.get(txPacket.TXInfo.MAC)
	if err != nil {
		return err
	}
	txpk, err := newTXPKFromTXPacket(txPacket)
	if err != nil {
		return err
	}
	pullResp := PullRespPacket{
		Payload: PullRespPayload{
			TXPK: txpk,
		},
	}
	bytes, err := pullResp.MarshalBinary()
	if err != nil {
		return err
	}
	b.udpSendChan <- udpPacket{
		data: bytes,
		addr: gw.addr,
	}
	return nil
}

func (b *Backend) readPackets() error {
	buf := make([]byte, 65507) // max udp data size
	for {
		i, addr, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			return err
		}
		data := make([]byte, i)
		copy(data, buf[:i])
		go func(data []byte) {
			if err := b.handlePacket(addr, data); err != nil {
				log.WithFields(log.Fields{
					"data_base64": base64.StdEncoding.EncodeToString(data),
					"addr":        addr,
				}).Errorf("could not handle packet: %s", err)
			}
		}(data)
	}
}

func (b *Backend) sendPackets() error {
	for p := range b.udpSendChan {
		pt, err := GetPacketType(p.data)
		if err != nil {
			log.WithFields(log.Fields{
				"addr":        p.addr,
				"data_base64": base64.StdEncoding.EncodeToString(p.data),
			}).Error("unknown packet type")
			continue
		}
		log.WithFields(log.Fields{
			"addr": p.addr,
			"type": pt,
		}).Info("outgoing gateway packet")

		if _, err := b.conn.WriteToUDP(p.data, p.addr); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) handlePacket(addr *net.UDPAddr, data []byte) error {
	pt, err := GetPacketType(data)
	if err != nil {
		return err
	}
	log.WithFields(log.Fields{
		"addr": addr,
		"type": pt,
	}).Info("incoming gateway packet")

	switch pt {
	case PushData:
		return b.handlePushData(addr, data)
	case PullData:
		return b.handlePullData(addr, data)
	default:
		return fmt.Errorf("unknown packet type: %s", pt)
	}
}

func (b *Backend) handlePullData(addr *net.UDPAddr, data []byte) error {
	var p PullDataPacket
	if err := p.UnmarshalBinary(data); err != nil {
		return err
	}
	ack := PullACKPacket{
		RandomToken: p.RandomToken,
	}
	bytes, err := ack.MarshalBinary()
	if err != nil {
		return err
	}

	err = b.gateways.set(p.GatewayMAC, gateway{
		addr:     addr,
		lastSeen: time.Now().UTC(),
	})
	if err != nil {
		return err
	}

	b.udpSendChan <- udpPacket{
		addr: addr,
		data: bytes,
	}
	return nil
}

func (b *Backend) handlePushData(addr *net.UDPAddr, data []byte) error {
	var p PushDataPacket
	if err := p.UnmarshalBinary(data); err != nil {
		return err
	}

	// ack the packet
	ack := PushACKPacket{
		RandomToken: p.RandomToken,
	}
	bytes, err := ack.MarshalBinary()
	if err != nil {
		return err
	}
	b.udpSendChan <- udpPacket{
		addr: addr,
		data: bytes,
	}

	// gateway stats
	if p.Payload.Stat != nil {
		b.handleStat(addr, p.GatewayMAC, *p.Payload.Stat)
	}

	// rx packets
	for _, rxpk := range p.Payload.RXPK {
		if err := b.handleRXPacket(addr, p.GatewayMAC, rxpk); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) handleStat(addr *net.UDPAddr, mac lorawan.EUI64, stat Stat) {
	gwStats := newGatewayStatsPacket(mac, stat)
	log.WithFields(log.Fields{
		"addr": addr,
		"mac":  mac,
	}).Info("stat packet received")
	b.statsChan <- gwStats
}

func (b *Backend) handleRXPacket(addr *net.UDPAddr, mac lorawan.EUI64, rxpk RXPK) error {
	logFields := log.Fields{
		"addr": addr,
		"mac":  mac,
		"data": rxpk.Data,
	}
	log.WithFields(logFields).Info("rxpk packet received")

	// decode packet
	rxPacket, err := newRXPacketFromRXPK(mac, rxpk)
	if err != nil {
		return err
	}

	// check CRC
	if rxPacket.RXInfo.CRCStatus != 1 {
		log.WithFields(logFields).Warningf("invalid packet CRC: %d", rxPacket.RXInfo.CRCStatus)
		return errors.New("invalid CRC")
	}
	b.rxChan <- rxPacket
	return nil
}

// newGatewayStatsPacket from Stat transforms a Semtech Stat packet into a
// loraserver.GatewayStatsPacket.
func newGatewayStatsPacket(mac lorawan.EUI64, stat Stat) loraserver.GatewayStatsPacket {
	return loraserver.GatewayStatsPacket{
		Time:                time.Time(stat.Time),
		MAC:                 mac,
		Latitude:            stat.Lati,
		Longitude:           stat.Long,
		Altitude:            float64(stat.Alti),
		RXPacketsReceived:   int(stat.RXNb),
		RXPacketsReceivedOK: int(stat.RXOK),
	}
}

// newRXPacketFromRXPK transforms a Semtech packet into a loraserver.RXPacket.
func newRXPacketFromRXPK(mac lorawan.EUI64, rxpk RXPK) (loraserver.RXPacket, error) {
	phy := lorawan.NewPHYPayload(true) // uplink payload
	bytes, err := base64.StdEncoding.DecodeString(rxpk.Data)
	if err != nil {
		return loraserver.RXPacket{}, fmt.Errorf("could not base64 decode data: %s", err)
	}
	if err := phy.UnmarshalBinary(bytes); err != nil {
		return loraserver.RXPacket{}, fmt.Errorf("could not unmarshal PHYPayload: %s", err)
	}

	rxPacket := loraserver.RXPacket{
		PHYPayload: phy,
		RXInfo: loraserver.RXInfo{
			MAC:        mac,
			Time:       time.Time(rxpk.Time),
			Timestamp:  rxpk.Tmst,
			Frequency:  rxpk.Freq,
			Channel:    uint(rxpk.Chan),
			RFChain:    uint(rxpk.RFCh),
			CRCStatus:  int(rxpk.Stat),
			Modulation: rxpk.Modu,
			DataRate: loraserver.DataRate{
				LoRa: rxpk.DatR.LoRa,
				FSK:  uint(rxpk.DatR.FSK),
			},
			CodeRate: rxpk.CodR,
			RSSI:     int(rxpk.RSSI),
			LoRaSNR:  rxpk.LSNR,
			Size:     uint(rxpk.Size),
		},
	}
	return rxPacket, nil
}

// newTXPKFromTXPacket transforms a loraserver.TXPacket into a Semtech
// compatible packet.
func newTXPKFromTXPacket(txPacket loraserver.TXPacket) (TXPK, error) {
	b, err := txPacket.PHYPayload.MarshalBinary()
	if err != nil {
		return TXPK{}, err
	}

	txpk := TXPK{
		Imme: txPacket.TXInfo.Immediately,
		Tmst: txPacket.TXInfo.Timestamp,
		Freq: txPacket.TXInfo.Frequency,
		RFCh: uint8(txPacket.TXInfo.RFChain),
		Powe: uint8(txPacket.TXInfo.Power),
		Modu: txPacket.TXInfo.DataRate.Modulation(),
		DatR: DatR{
			LoRa: txPacket.TXInfo.DataRate.LoRa,
			FSK:  uint32(txPacket.TXInfo.DataRate.FSK),
		},
		CodR: txPacket.TXInfo.CodeRate,
		FDev: uint16(txPacket.TXInfo.FrequencyDeviation),
		Size: uint16(len(b)),
		NCRC: txPacket.TXInfo.DisableCRC,
		Data: base64.RawStdEncoding.EncodeToString(b),
	}

	if txPacket.TXInfo.DataRate.Modulation() == "LORA" {
		txpk.IPol = true
	}

	return txpk, nil
}