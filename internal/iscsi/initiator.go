// Package iscsi implements a minimal pure-Go iSCSI initiator.
// It supports Login, SCSI WRITE(16) at arbitrary offsets, and Logout.
// No kernel modules, no iscsiadm — works on Mac, Linux, Docker.
package iscsi

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// iSCSI opcodes (initiator → target)
const (
	opLoginReq    = 0x03
	opSCSICmd     = 0x01
	opSCSIDataOut = 0x05
	opLogoutReq   = 0x06
	opNOPOut      = 0x00
)

// iSCSI opcodes (target → initiator)
const (
	opLoginResp   = 0x23
	opSCSIResp    = 0x21
	opSCSIDataIn  = 0x25
	opR2T         = 0x31
	opLogoutResp  = 0x26
	opNOPIn       = 0x20
	opReject      = 0x3f
)

// SCSI CDB opcodes
const (
	scsiWrite16    = 0x8A
	scsiWrite10    = 0x2A
	scsiReadCap16  = 0x9E
	scsiReadCap10  = 0x25
	scsiTestUnit   = 0x00
	scsiInquiry    = 0x12
)

const (
	bhsLen       = 48        // Basic Header Segment length
	maxSegment   = 256 * 1024 // 256 KB max data segment per PDU
)

// Initiator is a pure-Go iSCSI initiator for block-level writes.
type Initiator struct {
	conn          net.Conn
	initiatorName string
	targetName    string
	portal        string

	// Session state
	isid       [6]byte
	tsih       uint16
	cmdSN      uint32
	expStatSN  uint32
	itt        uint32 // initiator task tag counter

	// Negotiated parameters
	maxRecvSegment uint32 // target's MaxRecvDataSegmentLength
	maxBurstLength uint32
	firstBurst     uint32
	initialR2T     bool // if true, target sends R2T before we send data
	immediateData  bool // if true, we can send data with the command PDU

	// Disk parameters (from READ CAPACITY)
	sectorSize   uint32 // block size in bytes (512 or 4096)
	totalSectors uint64 // total number of sectors

	// Configurable limits
	maxWriteBytes int // max bytes per SCSI WRITE command (0 = use maxRecvSegment)

	mu sync.Mutex
}

// Portal returns the current portal address (may differ from original after redirect).
func (ini *Initiator) Portal() string {
	return ini.portal
}

// Config holds iSCSI connection parameters.
type Config struct {
	TargetIQN     string
	PortalIP      string
	PortalPort    int
	MaxWriteBytes int // max bytes per SCSI WRITE command (0 = use negotiated MaxRecvDataSegmentLength)
}

// NewInitiator creates a new pure-Go iSCSI initiator.
func NewInitiator(cfg Config) *Initiator {
	return &Initiator{
		initiatorName:  "iqn.2026-01.com.datamigrate:initiator",
		targetName:     cfg.TargetIQN,
		portal:         fmt.Sprintf("%s:%d", cfg.PortalIP, cfg.PortalPort),
		isid:           [6]byte{0x40, 0x00, 0x00, 0x00, 0x00, 0x01},
		cmdSN:          1,
		maxRecvSegment: maxSegment,
		maxBurstLength: 16 * 1024 * 1024, // 16 MB
		firstBurst:     maxSegment,
		initialR2T:     true,  // safe default: wait for R2T
		immediateData:  false, // safe default: no data with command
		sectorSize:     512,   // default, updated by ReadCapacity
		maxWriteBytes:  cfg.MaxWriteBytes,
	}
}

// DiscoverTargets performs a SendTargets discovery session to find available iSCSI targets.
// Nutanix creates per-disk virtual targets (e.g., "...vg-uuid-tgt0") that must be used
// instead of the base VG target IQN from the API.
func DiscoverTargets(portal string) ([]string, error) {
	log.Info().Str("portal", portal).Msg("starting SendTargets discovery")

	conn, err := net.DialTimeout("tcp", portal, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("discovery connect to %s: %w", portal, err)
	}
	defer conn.Close()

	ini := &Initiator{
		initiatorName: "iqn.2026-01.com.datamigrate:discovery",
		targetName:    "", // discovery session has no target
		portal:        portal,
		isid:          [6]byte{0x40, 0x00, 0x00, 0x00, 0x00, 0x02},
		cmdSN:         1,
		conn:          conn,
	}

	// Login with SessionType=Discovery
	kvPairs := []string{
		"InitiatorName=" + ini.initiatorName,
		"SessionType=Discovery",
		"AuthMethod=None",
	}
	data := []byte(strings.Join(kvPairs, "\x00") + "\x00")

	bhs := make([]byte, bhsLen)
	bhs[0] = 0x03 | 0x40 // Login Request, Immediate
	bhs[1] = 0x81         // T=1, CSG=00 (security), NSG=01 (operational)
	putDataSegmentLength(bhs, len(data))
	copy(bhs[8:14], ini.isid[:])
	ini.itt++
	binary.BigEndian.PutUint32(bhs[16:20], ini.itt)
	binary.BigEndian.PutUint32(bhs[24:28], ini.cmdSN)

	if err := ini.sendPDU(bhs, data); err != nil {
		return nil, fmt.Errorf("discovery login phase 1: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	respBHS, _, err := ini.recvPDU()
	if err != nil {
		return nil, fmt.Errorf("discovery login phase 1 response: %w", err)
	}
	if respBHS[36] != 0 {
		return nil, fmt.Errorf("discovery login phase 1 failed: class=%d detail=%d", respBHS[36], respBHS[37])
	}

	ini.tsih = binary.BigEndian.Uint16(respBHS[14:16])
	ini.expStatSN = binary.BigEndian.Uint32(respBHS[24:28]) + 1
	ini.cmdSN = binary.BigEndian.Uint32(respBHS[28:32])

	// Phase 2: operational negotiation for discovery
	opKV := []string{
		"HeaderDigest=None",
		"DataDigest=None",
		"MaxRecvDataSegmentLength=65536",
	}
	opData := []byte(strings.Join(opKV, "\x00") + "\x00")

	bhs2 := make([]byte, bhsLen)
	bhs2[0] = 0x03 | 0x40
	bhs2[1] = 0x87 // T=1, CSG=01, NSG=11
	putDataSegmentLength(bhs2, len(opData))
	copy(bhs2[8:14], ini.isid[:])
	binary.BigEndian.PutUint16(bhs2[14:16], ini.tsih)
	ini.itt++
	binary.BigEndian.PutUint32(bhs2[16:20], ini.itt)
	binary.BigEndian.PutUint32(bhs2[24:28], ini.cmdSN)
	binary.BigEndian.PutUint32(bhs2[28:32], ini.expStatSN)

	if err := ini.sendPDU(bhs2, opData); err != nil {
		return nil, fmt.Errorf("discovery login phase 2: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	respBHS2, _, err := ini.recvPDU()
	if err != nil {
		return nil, fmt.Errorf("discovery login phase 2 response: %w", err)
	}

	statusClass := respBHS2[36]
	// Handle redirect in discovery — just skip it and use the response data
	if statusClass != 0 && statusClass != 1 {
		return nil, fmt.Errorf("discovery login phase 2 failed: class=%d detail=%d", statusClass, respBHS2[37])
	}
	if statusClass == 0 {
		ini.expStatSN = binary.BigEndian.Uint32(respBHS2[24:28]) + 1
		ini.cmdSN = binary.BigEndian.Uint32(respBHS2[28:32])
	}

	// Send Text Request with SendTargets=All
	textData := []byte("SendTargets=All\x00")
	textBHS := make([]byte, bhsLen)
	textBHS[0] = 0x04 | 0x40 // Text Request, Immediate
	textBHS[1] = 0x80         // F=1
	putDataSegmentLength(textBHS, len(textData))
	ini.itt++
	binary.BigEndian.PutUint32(textBHS[16:20], ini.itt)
	binary.BigEndian.PutUint32(textBHS[20:24], 0xFFFFFFFF) // Target Transfer Tag
	binary.BigEndian.PutUint32(textBHS[24:28], ini.cmdSN)
	binary.BigEndian.PutUint32(textBHS[28:32], ini.expStatSN)

	if err := ini.sendPDU(textBHS, textData); err != nil {
		return nil, fmt.Errorf("SendTargets request: %w", err)
	}
	ini.cmdSN++

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, textResp, err := ini.recvPDU()
	if err != nil {
		return nil, fmt.Errorf("SendTargets response: %w", err)
	}

	// Parse discovered targets
	var targets []string
	parts := strings.Split(string(textResp), "\x00")
	for _, kv := range parts {
		if strings.HasPrefix(kv, "TargetName=") {
			target := strings.TrimPrefix(kv, "TargetName=")
			targets = append(targets, target)
			log.Info().Str("target", target).Msg("discovered iSCSI target")
		}
		if strings.HasPrefix(kv, "TargetAddress=") {
			log.Info().Str("address", strings.TrimPrefix(kv, "TargetAddress=")).Msg("discovered target address")
		}
	}

	log.Info().Int("count", len(targets)).Msg("SendTargets discovery complete")
	return targets, nil
}

// Connect establishes TCP connection and performs iSCSI login.
// If the base target IQN fails WRITE commands, try discovered virtual targets.
func (ini *Initiator) Connect() error {
	log.Info().
		Str("portal", ini.portal).
		Str("target", ini.targetName).
		Msg("connecting to iSCSI target (pure Go)")

	// First, discover available targets to find per-disk virtual target
	discovered, err := DiscoverTargets(ini.portal)
	if err != nil {
		log.Warn().Err(err).Msg("SendTargets discovery failed — using configured target IQN")
	} else if len(discovered) > 0 {
		// Look for a virtual target with -tgt suffix matching our base target
		for _, dt := range discovered {
			if strings.HasPrefix(dt, ini.targetName) && dt != ini.targetName {
				log.Info().
					Str("base_target", ini.targetName).
					Str("virtual_target", dt).
					Msg("using discovered virtual target instead of base VG target")
				ini.targetName = dt
				break
			}
		}
	}

	var conn net.Conn
	for attempt := 0; attempt < 5; attempt++ {
		conn, err = net.DialTimeout("tcp", ini.portal, 30*time.Second)
		if err == nil {
			break
		}
		wait := time.Duration(attempt+1) * 3 * time.Second
		log.Warn().
			Err(err).
			Int("attempt", attempt+1).
			Dur("retry_in", wait).
			Msg("TCP connect failed, retrying")
		time.Sleep(wait)
	}
	if err != nil {
		return fmt.Errorf("TCP connect to %s after 5 attempts: %w", ini.portal, err)
	}
	ini.conn = conn

	if err := ini.login(); err != nil {
		conn.Close()
		return fmt.Errorf("iSCSI login: %w", err)
	}

	log.Info().
		Str("target", ini.targetName).
		Str("portal", ini.portal).
		Uint32("max_recv_segment", ini.maxRecvSegment).
		Uint32("max_burst", ini.maxBurstLength).
		Uint32("first_burst", ini.firstBurst).
		Bool("initial_r2t", ini.initialR2T).
		Bool("immediate_data", ini.immediateData).
		Uint32("cmdSN", ini.cmdSN).
		Uint32("expStatSN", ini.expStatSN).
		Msg("iSCSI login successful — final negotiated params")

	// Read disk capacity to determine sector size
	if err := ini.readCapacity(); err != nil {
		log.Warn().Err(err).Msg("READ CAPACITY failed — using default 512-byte sectors")
		ini.sectorSize = 512
	}

	// Report LUNs to verify what's available
	ini.reportLUNs()

	// NOTE: Do NOT test-write to LBA 0 — it destroys the partition table!

	return nil
}

// reportLUNs sends REPORT LUNS command to discover available LUNs.
func (ini *Initiator) reportLUNs() {
	bhs := make([]byte, bhsLen)
	bhs[0] = opSCSICmd
	bhs[1] = 0xC1 // F=1, R=1, ATTR=SIMPLE

	binary.BigEndian.PutUint32(bhs[20:24], 256) // allocation length

	ini.itt++
	binary.BigEndian.PutUint32(bhs[16:20], ini.itt)
	binary.BigEndian.PutUint32(bhs[24:28], ini.cmdSN)
	binary.BigEndian.PutUint32(bhs[28:32], ini.expStatSN)

	cdb := bhs[32:48]
	cdb[0] = 0xA0 // REPORT LUNS
	binary.BigEndian.PutUint32(cdb[6:10], 256) // allocation length in CDB

	putDataSegmentLength(bhs, 0)

	log.Info().Msg("sending REPORT LUNS")

	if err := ini.sendPDU(bhs, nil); err != nil {
		log.Warn().Err(err).Msg("REPORT LUNS send failed")
		return
	}
	ini.cmdSN++

	for {
		ini.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		respBHS, respData, err := ini.recvPDU()
		if err != nil {
			log.Warn().Err(err).Msg("REPORT LUNS response failed")
			return
		}
		opcode := respBHS[0] & 0x3f
		if opcode == opSCSIDataIn {
			ini.expStatSN = binary.BigEndian.Uint32(respBHS[24:28]) + 1
			if len(respData) >= 8 {
				lunListLen := binary.BigEndian.Uint32(respData[0:4])
				numLUNs := lunListLen / 8
				log.Info().Uint32("lun_list_length", lunListLen).Uint32("num_luns", numLUNs).Msg("REPORT LUNS result")
				for i := uint32(0); i < numLUNs && int(8+i*8+8) <= len(respData); i++ {
					lunBytes := respData[8+i*8 : 8+i*8+8]
					log.Info().Hex("lun_bytes", lunBytes).Uint32("index", i).Msg("reported LUN")
				}
			}
			// Check S bit
			if respBHS[1]&0x01 != 0 {
				return
			}
			continue
		}
		if opcode == opSCSIResp {
			ini.expStatSN = binary.BigEndian.Uint32(respBHS[24:28]) + 1
			status := respBHS[3]
			if status != 0 {
				log.Warn().Uint8("status", status).Msg("REPORT LUNS failed")
			}
			return
		}
	}
}

// testWrite sends a tiny 1-block WRITE(10) to verify the target accepts writes.
func (ini *Initiator) testWrite() {
	testData := make([]byte, 512)
	// Write 1 block of zeros to LBA 0
	bhs := make([]byte, bhsLen)
	bhs[0] = opSCSICmd
	bhs[1] = 0x21 // W=1, ATTR=SIMPLE

	binary.BigEndian.PutUint32(bhs[20:24], 512) // Expected Data Transfer Length

	ini.itt++
	binary.BigEndian.PutUint32(bhs[16:20], ini.itt)
	binary.BigEndian.PutUint32(bhs[24:28], ini.cmdSN)
	binary.BigEndian.PutUint32(bhs[28:32], ini.expStatSN)

	cdb := bhs[32:48]
	cdb[0] = scsiWrite10
	// LBA = 0 (bytes 2-5 already zero)
	binary.BigEndian.PutUint16(cdb[7:9], 1) // Transfer length = 1 block

	bhs[1] |= 0x80 // F=1
	putDataSegmentLength(bhs, 0)

	log.Info().
		Hex("bhs_byte1", []byte{bhs[1]}).
		Hex("cdb", cdb[:10]).
		Hex("lun", bhs[8:16]).
		Msg("TEST: sending 1-block WRITE(10) to LBA 0")

	if err := ini.sendPDU(bhs, nil); err != nil {
		log.Error().Err(err).Msg("TEST WRITE send failed")
		return
	}
	ini.cmdSN++

	ini.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	respBHS, respData, err := ini.recvPDU()
	if err != nil {
		log.Error().Err(err).Msg("TEST WRITE response failed")
		return
	}

	opcode := respBHS[0] & 0x3f
	status := respBHS[3]
	response := respBHS[2]

	log.Info().
		Uint8("opcode", opcode).
		Uint8("response", response).
		Uint8("scsi_status", status).
		Msg("TEST WRITE response")

	if opcode == opR2T {
		r2tLen := binary.BigEndian.Uint32(respBHS[44:48])
		ttt := binary.BigEndian.Uint32(respBHS[20:24])
		log.Info().Uint32("r2t_length", r2tLen).Uint32("ttt", ttt).Msg("TEST WRITE got R2T — target accepts writes!")
		ini.expStatSN = binary.BigEndian.Uint32(respBHS[24:28]) + 1
		// Send the data
		if err := ini.sendDataOut(testData, 0, int(r2tLen), ttt, ini.itt); err != nil {
			log.Error().Err(err).Msg("TEST WRITE data-out failed")
			return
		}
		// Wait for SCSI response
		ini.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		finalBHS, _, err := ini.recvPDU()
		if err != nil {
			log.Error().Err(err).Msg("TEST WRITE final response failed")
			return
		}
		finalStatus := finalBHS[3]
		log.Info().Uint8("final_status", finalStatus).Msg("TEST WRITE completed")
		ini.expStatSN = binary.BigEndian.Uint32(finalBHS[24:28]) + 1
		return
	}

	if status != 0 {
		parseSenseData(respData, status)
		log.Error().Uint8("status", status).Msg("TEST WRITE failed — target rejects writes")
	} else {
		log.Info().Msg("TEST WRITE succeeded")
	}
	ini.expStatSN = binary.BigEndian.Uint32(respBHS[24:28]) + 1
}

// readCapacity issues a READ CAPACITY(10) command to determine sector size and disk capacity.
func (ini *Initiator) readCapacity() error {
	bhs := make([]byte, bhsLen)
	bhs[0] = opSCSICmd
	bhs[1] = 0xC1 // F=1, R=1 (read), W=0, ATTR=SIMPLE

	// Expected Data Transfer Length = 8 bytes (READ CAPACITY 10 response)
	binary.BigEndian.PutUint32(bhs[20:24], 8)

	ini.itt++
	binary.BigEndian.PutUint32(bhs[16:20], ini.itt)
	binary.BigEndian.PutUint32(bhs[24:28], ini.cmdSN)
	binary.BigEndian.PutUint32(bhs[28:32], ini.expStatSN)

	// CDB: READ CAPACITY(10)
	cdb := bhs[32:48]
	cdb[0] = scsiReadCap10

	putDataSegmentLength(bhs, 0)

	log.Info().Uint32("cmdSN", ini.cmdSN).Msg("sending READ CAPACITY(10)")

	if err := ini.sendPDU(bhs, nil); err != nil {
		return fmt.Errorf("sending READ CAPACITY: %w", err)
	}
	ini.cmdSN++

	// Read response — could be Data-In followed by SCSI Response, or just SCSI Response
	var capData []byte
	for {
		ini.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		respBHS, respData, err := ini.recvPDU()
		if err != nil {
			return fmt.Errorf("reading READ CAPACITY response: %w", err)
		}

		opcode := respBHS[0] & 0x3f
		switch opcode {
		case opSCSIDataIn:
			capData = respData
			ini.expStatSN = binary.BigEndian.Uint32(respBHS[24:28]) + 1
			// Check if F bit is set (last data PDU)
			if respBHS[1]&0x80 != 0 {
				// Check if S bit is set (status included in data-in)
				if respBHS[1]&0x01 != 0 {
					status := respBHS[3]
					if status != 0 {
						parseSenseData(respData, status)
						return fmt.Errorf("READ CAPACITY failed: SCSI status=%d", status)
					}
					goto parseCapacity
				}
			}
			continue

		case opSCSIResp:
			ini.expStatSN = binary.BigEndian.Uint32(respBHS[24:28]) + 1
			status := respBHS[3]
			if status != 0 {
				parseSenseData(respData, status)
				return fmt.Errorf("READ CAPACITY failed: SCSI status=%d", status)
			}
			goto parseCapacity

		default:
			log.Warn().Uint8("opcode", opcode).Msg("unexpected PDU during READ CAPACITY")
		}
	}

parseCapacity:
	if len(capData) >= 8 {
		lastLBA := binary.BigEndian.Uint32(capData[0:4])
		blockSize := binary.BigEndian.Uint32(capData[4:8])

		ini.sectorSize = blockSize
		ini.totalSectors = uint64(lastLBA) + 1

		log.Info().
			Uint32("last_lba", lastLBA).
			Uint32("block_size", blockSize).
			Uint64("total_sectors", ini.totalSectors).
			Uint64("capacity_bytes", ini.totalSectors*uint64(blockSize)).
			Uint64("capacity_gb", ini.totalSectors*uint64(blockSize)/(1024*1024*1024)).
			Msg("READ CAPACITY result — disk geometry")
	} else {
		log.Warn().Int("data_len", len(capData)).Msg("READ CAPACITY response too short")
	}
	return nil
}

// login performs the iSCSI login phase.
// It sends security + operational parameters in a single request (CSG=00 → NSG=11),
// which is the most compatible approach. If the target rejects this single-phase login,
// it falls back to a two-phase approach (security then operational).
func (ini *Initiator) login() error {
	// Try single-phase login first (CSG=00 → NSG=11 with all parameters)
	allKV := []string{
		"InitiatorName=" + ini.initiatorName,
		"TargetName=" + ini.targetName,
		"SessionType=Normal",
		"AuthMethod=None",
		"HeaderDigest=None",
		"DataDigest=None",
		fmt.Sprintf("MaxRecvDataSegmentLength=%d", maxSegment),
		"InitialR2T=Yes",
		"ImmediateData=No",
		fmt.Sprintf("MaxBurstLength=%d", ini.maxBurstLength),
		fmt.Sprintf("FirstBurstLength=%d", ini.firstBurst),
		"MaxConnections=1",
		"DefaultTime2Wait=0",
		"DefaultTime2Retain=0",
		"MaxOutstandingR2T=1",
		"DataPDUInOrder=Yes",
		"DataSequenceInOrder=Yes",
		"ErrorRecoveryLevel=0",
	}
	allData := []byte(strings.Join(allKV, "\x00") + "\x00")

	bhs := make([]byte, bhsLen)
	bhs[0] = opLoginReq | 0x40 // immediate bit
	bhs[1] = 0x83              // T=1 (transit), C=0, CSG=00 (security), NSG=11 (full feature)
	bhs[2] = 0x00              // version-max
	bhs[3] = 0x00              // version-min
	putDataSegmentLength(bhs, len(allData))
	copy(bhs[8:14], ini.isid[:])
	binary.BigEndian.PutUint16(bhs[14:16], 0) // TSIH=0 for new session
	ini.itt++
	binary.BigEndian.PutUint32(bhs[16:20], ini.itt)
	binary.BigEndian.PutUint32(bhs[24:28], ini.cmdSN)
	binary.BigEndian.PutUint32(bhs[28:32], ini.expStatSN)

	log.Info().
		Str("initiator_name", ini.initiatorName).
		Str("target_name", ini.targetName).
		Int("data_len", len(allData)).
		Str("phase", "single-phase CSG=00→NSG=11").
		Msg("sending iSCSI login request")

	if err := ini.sendPDU(bhs, allData); err != nil {
		return fmt.Errorf("sending login request: %w", err)
	}

	respBHS, respData, err := ini.recvPDU()
	if err != nil {
		return fmt.Errorf("reading login response: %w", err)
	}

	opcode := respBHS[0] & 0x3f
	respFlags := respBHS[1]
	transitBit := (respFlags & 0x80) != 0
	csg := (respFlags >> 2) & 0x03
	nsg := respFlags & 0x03

	log.Info().
		Uint8("opcode", opcode).
		Uint8("flags", respFlags).
		Bool("transit", transitBit).
		Uint8("csg", csg).
		Uint8("nsg", nsg).
		Uint8("status_class", respBHS[36]).
		Uint8("status_detail", respBHS[37]).
		Msg("iSCSI login response received")

	if opcode == opReject {
		log.Info().Msg("single-phase login rejected, trying two-phase login")
		// Close and reconnect for two-phase login
		ini.conn.Close()
		conn, err := net.DialTimeout("tcp", ini.portal, 30*time.Second)
		if err != nil {
			return fmt.Errorf("TCP reconnect for two-phase login: %w", err)
		}
		ini.conn = conn
		ini.itt = 0
		ini.cmdSN = 1
		ini.expStatSN = 0
		return ini.loginTwoPhase()
	}

	if opcode != opLoginResp {
		return fmt.Errorf("expected login response (0x%02x), got 0x%02x", opLoginResp, opcode)
	}
	if statusClass := respBHS[36]; statusClass != 0 {
		log.Warn().
			Uint8("status_class", statusClass).
			Uint8("status_detail", respBHS[37]).
			Msg("single-phase login failed, trying two-phase")
		ini.conn.Close()
		conn, err := net.DialTimeout("tcp", ini.portal, 30*time.Second)
		if err != nil {
			return fmt.Errorf("TCP reconnect for two-phase login: %w", err)
		}
		ini.conn = conn
		ini.itt = 0
		ini.cmdSN = 1
		ini.expStatSN = 0
		return ini.loginTwoPhase()
	}

	// Single-phase succeeded — check if we're in full feature phase
	ini.tsih = binary.BigEndian.Uint16(respBHS[14:16])
	singleStatSN := binary.BigEndian.Uint32(respBHS[24:28])
	ini.expStatSN = singleStatSN + 1
	singleExpCmdSN := binary.BigEndian.Uint32(respBHS[28:32])
	ini.cmdSN = singleExpCmdSN // Use target's ExpCmdSN, not increment
	ini.parseLoginParams(respData)

	if transitBit && nsg == 3 {
		log.Info().Msg("iSCSI single-phase login succeeded — in full feature phase")
		return nil
	}

	// Target accepted but wants more negotiation — continue with phase 2
	log.Info().Uint8("nsg", nsg).Msg("target wants further negotiation, sending phase 2")
	return ini.loginPhase2()
}

// loginTwoPhase performs a two-step login: security (CSG=00→NSG=01), then operational (CSG=01→NSG=11).
func (ini *Initiator) loginTwoPhase() error {
	// --- Phase 1: Security Negotiation (CSG=00 → NSG=01) ---
	secKV := []string{
		"InitiatorName=" + ini.initiatorName,
		"TargetName=" + ini.targetName,
		"SessionType=Normal",
		"AuthMethod=None",
	}
	secData := []byte(strings.Join(secKV, "\x00") + "\x00")

	bhs := make([]byte, bhsLen)
	bhs[0] = opLoginReq | 0x40
	bhs[1] = 0x81 // T=1, CSG=00 (security), NSG=01 (operational)
	bhs[2] = 0x00
	bhs[3] = 0x00
	putDataSegmentLength(bhs, len(secData))
	copy(bhs[8:14], ini.isid[:])
	binary.BigEndian.PutUint16(bhs[14:16], 0)
	ini.itt++
	binary.BigEndian.PutUint32(bhs[16:20], ini.itt)
	binary.BigEndian.PutUint32(bhs[24:28], ini.cmdSN)
	binary.BigEndian.PutUint32(bhs[28:32], ini.expStatSN)

	log.Info().Msg("sending iSCSI login phase 1 (security: CSG=00→NSG=01)")

	if err := ini.sendPDU(bhs, secData); err != nil {
		return fmt.Errorf("sending login phase 1: %w", err)
	}

	respBHS, respData1, err := ini.recvPDU()
	if err != nil {
		return fmt.Errorf("reading login phase 1 response: %w", err)
	}

	opcode := respBHS[0] & 0x3f
	respFlags := respBHS[1]
	transitBit := (respFlags & 0x80) != 0
	nsg := respFlags & 0x03

	statSN := binary.BigEndian.Uint32(respBHS[24:28])
	expCmdSN := binary.BigEndian.Uint32(respBHS[28:32])
	maxCmdSN := binary.BigEndian.Uint32(respBHS[32:36])

	log.Info().
		Uint8("opcode", opcode).
		Uint8("flags", respFlags).
		Bool("transit", transitBit).
		Uint8("nsg", nsg).
		Uint8("status_class", respBHS[36]).
		Uint8("status_detail", respBHS[37]).
		Uint32("statSN", statSN).
		Uint32("expCmdSN", expCmdSN).
		Uint32("maxCmdSN", maxCmdSN).
		Uint16("tsih", binary.BigEndian.Uint16(respBHS[14:16])).
		Str("resp_data", string(respData1)).
		Msg("phase 1 response")

	if opcode != opLoginResp {
		return fmt.Errorf("phase 1: expected login response (0x%02x), got 0x%02x", opLoginResp, opcode)
	}
	if statusClass := respBHS[36]; statusClass != 0 {
		return fmt.Errorf("phase 1 failed: status class=%d detail=%d", statusClass, respBHS[37])
	}

	ini.tsih = binary.BigEndian.Uint16(respBHS[14:16])
	ini.expStatSN = statSN + 1 // ExpStatSN = last received StatSN + 1
	ini.parseLoginParams(respData1)
	ini.cmdSN = expCmdSN // Use the ExpCmdSN from target

	// If target already transitioned to full feature phase, we're done
	if transitBit && nsg == 3 {
		log.Info().Msg("target transitioned to full feature phase after phase 1")
		return nil
	}

	// --- Phase 2: Operational Negotiation (CSG=01 → NSG=11) ---
	return ini.loginPhase2()
}

// loginPhase2 sends operational parameters (CSG=01 → NSG=11).
// Handles iSCSI target redirect (status class=1, detail=1) by reconnecting
// to the TargetAddress specified in the response.
func (ini *Initiator) loginPhase2() error {
	opKV := []string{
		"HeaderDigest=None",
		"DataDigest=None",
		fmt.Sprintf("MaxRecvDataSegmentLength=%d", maxSegment),
		"InitialR2T=Yes",
		"ImmediateData=No",
		fmt.Sprintf("MaxBurstLength=%d", ini.maxBurstLength),
		fmt.Sprintf("FirstBurstLength=%d", ini.firstBurst),
		"MaxConnections=1",
		"DefaultTime2Wait=0",
		"DefaultTime2Retain=0",
		"MaxOutstandingR2T=1",
		"DataPDUInOrder=Yes",
		"DataSequenceInOrder=Yes",
		"ErrorRecoveryLevel=0",
	}
	opData := []byte(strings.Join(opKV, "\x00") + "\x00")

	bhs := make([]byte, bhsLen)
	bhs[0] = opLoginReq | 0x40
	bhs[1] = 0x87 // T=1, CSG=01 (operational), NSG=11 (full feature)
	bhs[2] = 0x00
	bhs[3] = 0x00
	putDataSegmentLength(bhs, len(opData))
	copy(bhs[8:14], ini.isid[:])
	binary.BigEndian.PutUint16(bhs[14:16], ini.tsih)
	ini.itt++
	binary.BigEndian.PutUint32(bhs[16:20], ini.itt)
	binary.BigEndian.PutUint32(bhs[24:28], ini.cmdSN)
	binary.BigEndian.PutUint32(bhs[28:32], ini.expStatSN)

	log.Info().
		Uint32("cmdSN", ini.cmdSN).
		Uint32("expStatSN", ini.expStatSN).
		Uint16("tsih", ini.tsih).
		Msg("sending iSCSI login phase 2 (operational: CSG=01→NSG=11)")

	if err := ini.sendPDU(bhs, opData); err != nil {
		return fmt.Errorf("sending login phase 2: %w", err)
	}

	respBHS, respData, err := ini.recvPDU()
	if err != nil {
		return fmt.Errorf("reading login phase 2 response: %w", err)
	}

	opcode := respBHS[0] & 0x3f
	respFlags := respBHS[1]
	statSN := binary.BigEndian.Uint32(respBHS[24:28])

	log.Info().
		Uint8("opcode", opcode).
		Uint8("flags", respFlags).
		Uint8("status_class", respBHS[36]).
		Uint8("status_detail", respBHS[37]).
		Uint32("statSN", statSN).
		Str("resp_data", string(respData)).
		Msg("phase 2 response")

	if opcode != opLoginResp {
		return fmt.Errorf("phase 2: expected login response (0x%02x), got 0x%02x", opLoginResp, opcode)
	}

	statusClass := respBHS[36]
	statusDetail := respBHS[37]

	// Handle redirect: status class=1 means target wants us to connect elsewhere
	if statusClass == 1 {
		redirectAddr := ini.extractTargetAddress(respData)
		if redirectAddr == "" {
			return fmt.Errorf("phase 2: target redirect (class=%d detail=%d) but no TargetAddress in response", statusClass, statusDetail)
		}

		// Nutanix redirects to CVM Stargate port (e.g., 172.16.1.2:3205).
		// Port 3205 is the Stargate iSCSI data port on individual CVMs.
		log.Info().
			Str("current_portal", ini.portal).
			Str("redirect_to", redirectAddr).
			Msg("iSCSI target redirect — reconnecting to CVM Stargate")

		ini.conn.Close()

		conn, err := net.DialTimeout("tcp", redirectAddr, 30*time.Second)
		if err != nil {
			return fmt.Errorf("connecting to redirected portal %s: %w", redirectAddr, err)
		}
		ini.portal = redirectAddr
		ini.conn = conn

		log.Info().Str("connected_to", ini.portal).Msg("connected to redirected CVM")

		// Reset login state and redo full login on new connection
		ini.itt = 0
		ini.cmdSN = 1
		ini.expStatSN = 0
		ini.tsih = 0
		return ini.loginTwoPhase()
	}

	if statusClass != 0 {
		return fmt.Errorf("phase 2 failed: status class=%d detail=%d", statusClass, statusDetail)
	}

	ini.expStatSN = statSN + 1
	ini.parseLoginParams(respData)
	// Use ExpCmdSN from target — login PDUs are immediate and don't advance CmdSN
	p2ExpCmdSN := binary.BigEndian.Uint32(respBHS[28:32])
	ini.cmdSN = p2ExpCmdSN

	log.Info().
		Uint32("cmdSN", ini.cmdSN).
		Uint32("expStatSN", ini.expStatSN).
		Msg("iSCSI two-phase login succeeded — in full feature phase")
	return nil
}

// extractTargetAddress parses TargetAddress from iSCSI login response data.
// Format: "TargetAddress=ip:port" or "TargetAddress=ip:port,portalgroup"
func (ini *Initiator) extractTargetAddress(data []byte) string {
	parts := strings.Split(string(data), "\x00")
	for _, kv := range parts {
		if strings.HasPrefix(kv, "TargetAddress=") {
			addr := strings.TrimPrefix(kv, "TargetAddress=")
			// Remove portal group tag if present (e.g., "172.16.1.2:3205,1")
			if idx := strings.Index(addr, ","); idx != -1 {
				addr = addr[:idx]
			}
			return addr
		}
	}
	return ""
}

// parseLoginParams extracts negotiated parameters from login response.
func (ini *Initiator) parseLoginParams(data []byte) {
	pairs := strings.Split(string(data), "\x00")
	for _, pair := range pairs {
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := kv[0], kv[1]
		switch key {
		case "MaxRecvDataSegmentLength":
			var v uint32
			fmt.Sscanf(val, "%d", &v)
			if v > 0 {
				ini.maxRecvSegment = v
			}
			log.Info().Uint32("value", ini.maxRecvSegment).Msg("negotiated MaxRecvDataSegmentLength")
		case "MaxBurstLength":
			var v uint32
			fmt.Sscanf(val, "%d", &v)
			if v > 0 {
				ini.maxBurstLength = v
			}
		case "FirstBurstLength":
			var v uint32
			fmt.Sscanf(val, "%d", &v)
			if v > 0 {
				ini.firstBurst = v
			}
		case "InitialR2T":
			ini.initialR2T = (val == "Yes")
			log.Info().Bool("value", ini.initialR2T).Msg("negotiated InitialR2T")
		case "ImmediateData":
			ini.immediateData = (val == "Yes")
			log.Info().Bool("value", ini.immediateData).Msg("negotiated ImmediateData")
		case "TargetPortalGroupTag":
			log.Debug().Str("tpgt", val).Msg("target portal group tag")
		case "TargetAddress":
			log.Info().Str("addr", val).Msg("target address (redirect)")
		}
	}
}

// WriteAt writes data at the specified byte offset on the iSCSI target LUN.
// Large writes are split into multiple SCSI WRITE(16) commands, each within MaxBurstLength.
func (ini *Initiator) WriteAt(data []byte, offset int64) error {
	ini.mu.Lock()
	defer ini.mu.Unlock()

	dataLen := len(data)
	if dataLen == 0 {
		return nil
	}

	// Split into chunks sized for iSCSI efficiency.
	// Nutanix Stargate rejects large transfer lengths (32768 blocks = 16MB fails).
	// Configurable via plan YAML (iscsi_chunk_bytes), defaults to MaxRecvDataSegmentLength (typically 1MB).
	maxWrite := ini.maxWriteBytes
	if maxWrite <= 0 {
		maxWrite = int(ini.maxRecvSegment)
	}
	if maxWrite <= 0 {
		maxWrite = 1024 * 1024 // 1 MB default
	}

	for writeOffset := 0; writeOffset < dataLen; writeOffset += maxWrite {
		end := writeOffset + maxWrite
		if end > dataLen {
			end = dataLen
		}
		chunk := data[writeOffset:end]
		chunkOffset := offset + int64(writeOffset)

		if err := ini.writeAtSingle(chunk, chunkOffset); err != nil {
			return err
		}
	}
	return nil
}

// writeAtSingle sends a single SCSI WRITE(16) command for data within MaxBurstLength.
func (ini *Initiator) writeAtSingle(data []byte, offset int64) error {
	dataLen := len(data)
	if dataLen == 0 {
		return nil
	}

	// Pad data to sector boundary — SCSI requires transfer length to match
	// the actual data sent. Without padding, the target waits for bytes
	// that never arrive, causing a timeout.
	sectorSize := int(ini.sectorSize)
	remainder := dataLen % sectorSize
	if remainder != 0 {
		padding := sectorSize - remainder
		data = append(data, make([]byte, padding)...)
		dataLen = len(data)
	}

	// Convert byte offset to LBA
	lba := uint64(offset) / uint64(ini.sectorSize)
	transferBlocks := uint32(dataLen / sectorSize)

	// Build SCSI Command PDU for WRITE(16)
	bhs := make([]byte, bhsLen)
	bhs[0] = opSCSICmd
	bhs[1] = 0x21 // W=1 (write), ATTR=SIMPLE(001) — Nutanix rejects UNTAGGED(000)

	// LUN (bytes 8-15) — LUN 0
	// (already zero)

	// Initiator Task Tag — save for this command's Data-Out PDUs.
	// Must NOT use ini.itt later because sendNOPOut may increment it mid-command.
	ini.itt++
	cmdITT := ini.itt
	binary.BigEndian.PutUint32(bhs[16:20], cmdITT)

	// Expected Data Transfer Length
	binary.BigEndian.PutUint32(bhs[20:24], uint32(dataLen))

	// CmdSN
	binary.BigEndian.PutUint32(bhs[24:28], ini.cmdSN)

	// ExpStatSN
	binary.BigEndian.PutUint32(bhs[28:32], ini.expStatSN)

	// CDB (SCSI Command Descriptor Block) — WRITE(10) at bytes 32-41
	// WRITE(10) is more widely supported than WRITE(16)
	// Max LBA: 2^32 (2TB with 512-byte sectors — sufficient for most disks)
	// Max Transfer: 65535 blocks (32MB with 512-byte sectors)
	cdb := bhs[32:48]
	cdb[0] = scsiWrite10
	binary.BigEndian.PutUint32(cdb[2:6], uint32(lba))
	binary.BigEndian.PutUint16(cdb[7:9], uint16(transferBlocks))

	log.Debug().
		Uint64("lba", lba).
		Uint32("blocks", transferBlocks).
		Int("data_len", dataLen).
		Int64("offset", offset).
		Uint32("cmdSN", ini.cmdSN).
		Msg("sending SCSI WRITE(10)")

	sent := 0

	if ini.immediateData {
		// Send immediate data with the command PDU (up to FirstBurstLength)
		immLen := dataLen
		if immLen > int(ini.firstBurst) {
			immLen = int(ini.firstBurst)
		}
		if immLen > int(ini.maxRecvSegment) {
			immLen = int(ini.maxRecvSegment)
		}

		bhs[1] |= 0x80 // F=1 (final command PDU)
		putDataSegmentLength(bhs, immLen)

		log.Info().Int("immediate_bytes", immLen).Msg("sending immediate data with WRITE command")

		if err := ini.sendPDU(bhs, data[:immLen]); err != nil {
			return fmt.Errorf("sending SCSI WRITE command with immediate data: %w", err)
		}
		sent = immLen
	} else {
		// No immediate data
		bhs[1] |= 0x80 // F=1 (final command PDU)
		putDataSegmentLength(bhs, 0)

		if err := ini.sendPDU(bhs, nil); err != nil {
			return fmt.Errorf("sending SCSI WRITE command: %w", err)
		}
	}
	ini.cmdSN++

	if !ini.initialR2T && sent < dataLen {
		// Send unsolicited data (up to FirstBurstLength) without waiting for R2T
		unsolLen := int(ini.firstBurst) - sent
		if unsolLen > dataLen-sent {
			unsolLen = dataLen - sent
		}
		if unsolLen > 0 {
			log.Info().Int("unsolicited_bytes", unsolLen).Int("from_offset", sent).Msg("sending unsolicited first burst data")
			// Use TTT=0xFFFFFFFF for unsolicited data
			if err := ini.sendDataOut(data, sent, unsolLen, 0xFFFFFFFF, cmdITT); err != nil {
				return fmt.Errorf("sending unsolicited data: %w", err)
			}
			sent += unsolLen
		}
	}

	log.Debug().Int("sent_so_far", sent).Int("total", dataLen).Msg("waiting for R2T/response")
	for sent < dataLen {
		// Set read deadline to detect stuck connections
		ini.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		respBHS, respDataWrite, err := ini.recvPDU()
		if err != nil {
			return fmt.Errorf("waiting for R2T/response: %w", err)
		}

		opcode := respBHS[0] & 0x3f
		log.Debug().Uint8("opcode", opcode).Int("sent", sent).Msg("received PDU during write")

		switch opcode {
		case opR2T:
			r2tOffset := binary.BigEndian.Uint32(respBHS[40:44])
			r2tLength := binary.BigEndian.Uint32(respBHS[44:48])
			ttt := binary.BigEndian.Uint32(respBHS[20:24])
			r2tStatSN := binary.BigEndian.Uint32(respBHS[24:28])

			log.Debug().Uint32("r2t_offset", r2tOffset).Uint32("r2t_length", r2tLength).Msg("R2T received")

			ini.expStatSN = r2tStatSN + 1

			// Send Data-Out PDUs for this R2T
			if err := ini.sendDataOut(data, int(r2tOffset), int(r2tLength), ttt, cmdITT); err != nil {
				return fmt.Errorf("sending data-out: %w", err)
			}
			sent = int(r2tOffset) + int(r2tLength)
			if sent > dataLen {
				sent = dataLen
			}
			log.Debug().Int("sent", sent).Int("total", dataLen).Msg("data-out sent for R2T")

		case opSCSIResp:
			scsiStatSN := binary.BigEndian.Uint32(respBHS[24:28])
			ini.expStatSN = scsiStatSN + 1
			status := respBHS[3] // SCSI status
			response := respBHS[2]
			log.Debug().Uint8("scsi_status", status).Msg("SCSI response received")
			if response != 0 || status != 0 {
				parseSenseData(respDataWrite, status)
				return fmt.Errorf("SCSI WRITE failed: response=%d status=%d", response, status)
			}
			return nil // success

		case opNOPIn:
			// Handle NOP-In (keepalive) — respond with NOP-Out.
			// CRITICAL: sendNOPOut increments ini.itt, but we use cmdITT
			// (saved at command start) for Data-Out PDUs, so ITT stays correct.
			ttt := binary.BigEndian.Uint32(respBHS[20:24])
			log.Info().Uint32("ttt", ttt).Uint32("cmdITT", cmdITT).Uint32("itt_before", ini.itt).Msg("NOP-In during WRITE — responding with saved cmdITT")
			if ttt != 0xffffffff {
				ini.sendNOPOut(ttt)
			}
			log.Info().Uint32("itt_after", ini.itt).Uint32("cmdITT", cmdITT).Msg("NOP-Out sent, cmdITT unchanged")

		default:
			return fmt.Errorf("unexpected opcode 0x%02x during write", opcode)
		}
	}

	// After all data sent, wait for final SCSI Response
	for {
		ini.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		respBHS, respDataFinal, err := ini.recvPDU()
		if err != nil {
			return fmt.Errorf("waiting for SCSI response: %w", err)
		}
		opcode := respBHS[0] & 0x3f
		log.Debug().Uint8("opcode", opcode).Msg("waiting for final response")
		switch opcode {
		case opSCSIResp:
			finalStatSN := binary.BigEndian.Uint32(respBHS[24:28])
			ini.expStatSN = finalStatSN + 1
			status := respBHS[3]
			response := respBHS[2]
			log.Debug().Uint8("scsi_status", status).Msg("final SCSI response")
			if response != 0 || status != 0 {
				parseSenseData(respDataFinal, status)
				return fmt.Errorf("SCSI WRITE failed: response=%d status=%d", response, status)
			}
			return nil
		case opNOPIn:
			ttt := binary.BigEndian.Uint32(respBHS[20:24])
			if ttt != 0xffffffff {
				ini.sendNOPOut(ttt)
			}
		default:
			return fmt.Errorf("unexpected opcode 0x%02x waiting for response", opcode)
		}
	}
}

// sendDataOut sends Data-Out PDUs for an R2T.
// cmdITT is the Initiator Task Tag from the original SCSI command — must match exactly.
func (ini *Initiator) sendDataOut(fullData []byte, r2tOffset, r2tLength int, ttt uint32, cmdITT uint32) error {
	dataSN := uint32(0)
	remaining := r2tLength
	pos := r2tOffset

	for remaining > 0 {
		chunkSize := int(ini.maxRecvSegment)
		if chunkSize > remaining {
			chunkSize = remaining
		}
		if pos+chunkSize > len(fullData) {
			chunkSize = len(fullData) - pos
		}
		if chunkSize <= 0 {
			break
		}

		chunk := fullData[pos : pos+chunkSize]

		bhs := make([]byte, bhsLen)
		bhs[0] = opSCSIDataOut

		// F bit: set if this is the last Data-Out for this R2T
		if remaining-chunkSize <= 0 {
			bhs[1] = 0x80 // F=1
		}

		putDataSegmentLength(bhs, chunkSize)

		// Initiator Task Tag (must match the original SCSI command's ITT)
		binary.BigEndian.PutUint32(bhs[16:20], cmdITT)

		// Target Transfer Tag (from R2T)
		binary.BigEndian.PutUint32(bhs[20:24], ttt)

		// ExpStatSN
		binary.BigEndian.PutUint32(bhs[28:32], ini.expStatSN)

		// DataSN
		binary.BigEndian.PutUint32(bhs[36:40], dataSN)

		// Buffer Offset
		binary.BigEndian.PutUint32(bhs[40:44], uint32(pos))

		if err := ini.sendPDU(bhs, chunk); err != nil {
			return fmt.Errorf("sending data-out at offset %d: %w", pos, err)
		}

		pos += chunkSize
		remaining -= chunkSize
		dataSN++
	}
	return nil
}

// sendNOPOut responds to a NOP-In from the target.
func (ini *Initiator) sendNOPOut(ttt uint32) {
	bhs := make([]byte, bhsLen)
	bhs[0] = opNOPOut | 0x40 // immediate
	bhs[1] = 0x80            // F=1

	ini.itt++
	binary.BigEndian.PutUint32(bhs[16:20], ini.itt)
	binary.BigEndian.PutUint32(bhs[20:24], ttt)
	binary.BigEndian.PutUint32(bhs[24:28], ini.cmdSN)
	binary.BigEndian.PutUint32(bhs[28:32], ini.expStatSN)

	_ = ini.sendPDU(bhs, nil)
}

// parseSenseData extracts and logs SCSI sense data from a response PDU.
// SCSI status 2 = CHECK CONDITION, sense data explains the error.
func parseSenseData(data []byte, scsiStatus uint8) {
	statusName := "UNKNOWN"
	switch scsiStatus {
	case 0:
		statusName = "GOOD"
	case 2:
		statusName = "CHECK_CONDITION"
	case 4:
		statusName = "CONDITION_MET"
	case 8:
		statusName = "BUSY"
	case 0x18:
		statusName = "RESERVATION_CONFLICT"
	case 0x28:
		statusName = "TASK_SET_FULL"
	case 0x30:
		statusName = "ACA_ACTIVE"
	case 0x40:
		statusName = "TASK_ABORTED"
	}

	if len(data) < 2 {
		log.Error().
			Uint8("scsi_status", scsiStatus).
			Str("status_name", statusName).
			Int("sense_data_len", len(data)).
			Hex("raw_data", data).
			Msg("SCSI error — no sense data available")
		return
	}

	// iSCSI wraps sense data with a 2-byte length prefix
	senseLen := int(binary.BigEndian.Uint16(data[0:2]))
	senseData := data[2:]
	if senseLen > len(senseData) {
		senseLen = len(senseData)
	}
	senseData = senseData[:senseLen]

	senseKey := uint8(0)
	asc := uint8(0)
	ascq := uint8(0)

	if len(senseData) >= 3 {
		// Fixed format sense data
		if senseData[0]&0x7E == 0x70 {
			// Response code 0x70 or 0x71 (fixed format)
			senseKey = senseData[2] & 0x0F
			if len(senseData) >= 13 {
				asc = senseData[12]
			}
			if len(senseData) >= 14 {
				ascq = senseData[13]
			}
		} else if senseData[0]&0x7E == 0x72 {
			// Response code 0x72 or 0x73 (descriptor format)
			senseKey = senseData[1] & 0x0F
			asc = senseData[2]
			ascq = senseData[3]
		}
	}

	senseKeyName := "UNKNOWN"
	switch senseKey {
	case 0x0:
		senseKeyName = "NO_SENSE"
	case 0x1:
		senseKeyName = "RECOVERED_ERROR"
	case 0x2:
		senseKeyName = "NOT_READY"
	case 0x3:
		senseKeyName = "MEDIUM_ERROR"
	case 0x4:
		senseKeyName = "HARDWARE_ERROR"
	case 0x5:
		senseKeyName = "ILLEGAL_REQUEST"
	case 0x6:
		senseKeyName = "UNIT_ATTENTION"
	case 0x7:
		senseKeyName = "DATA_PROTECT"
	case 0xB:
		senseKeyName = "ABORTED_COMMAND"
	}

	log.Error().
		Uint8("scsi_status", scsiStatus).
		Str("status_name", statusName).
		Uint8("sense_key", senseKey).
		Str("sense_key_name", senseKeyName).
		Uint8("asc", asc).
		Uint8("ascq", ascq).
		Int("sense_data_len", senseLen).
		Hex("sense_data", senseData).
		Msg("SCSI error with sense data")
}

// Disconnect performs iSCSI logout and closes the TCP connection.
func (ini *Initiator) Disconnect() error {
	if ini.conn == nil {
		return nil
	}

	ini.mu.Lock()
	defer ini.mu.Unlock()

	// Send Logout Request
	bhs := make([]byte, bhsLen)
	bhs[0] = opLogoutReq | 0x40 // immediate
	bhs[1] = 0x80               // F=1, reason=close session

	ini.itt++
	binary.BigEndian.PutUint32(bhs[16:20], ini.itt)
	binary.BigEndian.PutUint32(bhs[24:28], ini.cmdSN)
	binary.BigEndian.PutUint32(bhs[28:32], ini.expStatSN)

	if err := ini.sendPDU(bhs, nil); err != nil {
		ini.conn.Close()
		ini.conn = nil
		return fmt.Errorf("sending logout: %w", err)
	}

	// Read logout response (best-effort)
	ini.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	respBHS, _, err := ini.recvPDU()
	if err == nil {
		opcode := respBHS[0] & 0x3f
		if opcode == opLogoutResp {
			log.Debug().Msg("iSCSI logout response received")
		}
	}

	ini.conn.Close()
	ini.conn = nil
	log.Info().Str("target", ini.targetName).Msg("iSCSI disconnected")
	return nil
}

// sendPDU writes a BHS + optional data segment to the TCP connection.
func (ini *Initiator) sendPDU(bhs []byte, data []byte) error {
	// Write BHS
	if _, err := ini.conn.Write(bhs); err != nil {
		return err
	}

	// Write data segment
	if len(data) > 0 {
		if _, err := ini.conn.Write(data); err != nil {
			return err
		}
		// Pad to 4-byte boundary
		pad := (4 - len(data)%4) % 4
		if pad > 0 {
			if _, err := ini.conn.Write(make([]byte, pad)); err != nil {
				return err
			}
		}
	}
	return nil
}

// recvPDU reads a BHS + data segment from the TCP connection.
func (ini *Initiator) recvPDU() ([]byte, []byte, error) {
	bhs := make([]byte, bhsLen)
	if _, err := io.ReadFull(ini.conn, bhs); err != nil {
		return nil, nil, fmt.Errorf("reading BHS: %w", err)
	}

	dataLen := getDataSegmentLength(bhs)
	var data []byte
	if dataLen > 0 {
		// Read data + padding
		padLen := (4 - dataLen%4) % 4
		buf := make([]byte, dataLen+padLen)
		if _, err := io.ReadFull(ini.conn, buf); err != nil {
			return bhs, nil, fmt.Errorf("reading data segment: %w", err)
		}
		data = buf[:dataLen]
	}

	return bhs, data, nil
}

// putDataSegmentLength sets the data segment length in the BHS (bytes 5-7).
func putDataSegmentLength(bhs []byte, length int) {
	bhs[5] = byte(length >> 16)
	bhs[6] = byte(length >> 8)
	bhs[7] = byte(length)
}

// getDataSegmentLength reads the data segment length from the BHS (bytes 5-7).
func getDataSegmentLength(bhs []byte) int {
	return int(bhs[5])<<16 | int(bhs[6])<<8 | int(bhs[7])
}
