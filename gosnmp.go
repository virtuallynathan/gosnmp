// Copyright 2012-2014 The GoSNMP Authors. All rights reserved.  Use of this
// source code is governed by a BSD-style license that can be found in the
// LICENSE file.

// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gosnmp

import (
	"fmt"
	"io/ioutil"
	"log"
	"math/big"
	"math/rand"
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	maxOids               = 60             // maxOids is the maximum number of oids allowed in a Get()
	baseOid               = ".1.3.6.1.2.1" // Base OID for MIB-2 defined SNMP variables
	defaultMaxRepetitions = 50             // Java SNMP uses 50, snmp-net uses 10

)

// LoggingDisabled is set if the Logger is nil, short circuits any 'slog' calls
var LoggingDisabled bool

// GoSNMP represents GoSNMP library state
type GoSNMP struct {
	Target    string        // Target is an ipv4 address
	Port      uint16        // Port is a udp port
	Community string        // Community is an SNMP Community string
	Version   SnmpVersion   // Version is an SNMP Version
	Timeout   time.Duration // Timeout is the timeout for the SNMP Query
	Retries   int           // Set the number of retries to attempt within timeout.
	Conn      net.Conn      // Conn is net connection to use, typically establised using GoSNMP.Connect()

	// Logger is the GoSNMP.Logger to use for debugging. If nil, debugging
	// output will be discarded (/dev/null). For verbose logging to stdout:
	// x.Logger = log.New(os.Stdout, "", 0)
	Logger Logger

	MaxRepetitions int        // MaxRepititions sets the GETBULK max-repetitions used by BulkWalk* (default: 50)
	NonRepeaters   int        // NonRepeaters sets the GETBULK max-repeaters used by BulkWalk* (default: 0 as per RFC 1905)
	requestID      uint32     // Internal - used to sync requests to response
	random         *rand.Rand // Internal - used to sync requests to responses

}

// The default connection settings
var Default = &GoSNMP{
	Port:      161,
	Community: "public",
	Version:   Version2c,
	Timeout:   time.Duration(2) * time.Second,
	Retries:   3,
}

// SnmpPDU will be used when doing SNMP Set's
type SnmpPDU struct {
	Name  string      // Name is an oid in string format eg ".1.3.6.1.4.9.27"
	Type  Asn1BER     // The type of the value eg Integer
	Value interface{} // The value to be set by the SNMP set
}

// Asn1BER is the type of the SNMP PDU
type Asn1BER byte

// Asn1BER's - http://www.ietf.org/rfc/rfc1442.txt
const (
	EndOfContents     Asn1BER = 0x00
	UnknownType               = 0x00 // TODO these should all be type Asn1BER, however
	Boolean                   = 0x01 // tests fail if implemented. See for example
	Integer                   = 0x02 /// http://stackoverflow.com/questions/5037610/typed-constant-declaration-list.
	BitString                 = 0x03
	OctetString               = 0x04
	Null                      = 0x05
	ObjectIdentifier          = 0x06
	ObjectDescription         = 0x07
	IPAddress                 = 0x40
	Counter32                 = 0x41
	Gauge32                   = 0x42
	TimeTicks                 = 0x43
	Opaque                    = 0x44
	NsapAddress               = 0x45
	Counter64                 = 0x46
	Uinteger32                = 0x47
	NoSuchObject              = 0x80
	NoSuchInstance            = 0x81
	EndOfMibView              = 0x82
)

//
// Public Functions (main interface)
//

// Connect initiates a connection to the target host
func (x *GoSNMP) Connect() error {
	if x.Logger == nil {
		LoggingDisabled = true
	}
	Conn, err := net.DialTimeout("udp", fmt.Sprintf("%s:%d", x.Target, x.Port), x.Timeout)
	if err == nil {
		x.Conn = Conn
	} else {
		return fmt.Errorf("Error establishing connection to host: %s\n", err.Error())
	}
	if x.random == nil {
		x.random = rand.New(rand.NewSource(time.Now().UTC().UnixNano()))
	}
	x.requestID = x.random.Uint32()
	return nil
}

// send sends the SNMP packet generated in the other functions and recieves a result
func (x *GoSNMP) send(pdus []SnmpPDU, packetOut *SnmpPacket) (result *SnmpPacket, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("recover: %v", e)
		}
	}()

	if x.Conn == nil {
		return nil, fmt.Errorf("&GoSNMP.Conn is missing. Provide a connection or use Connect()")
	}

	if x.Logger == nil {
		x.Logger = log.New(ioutil.Discard, "", 0)
	}
	slog = x.Logger // global variable for debug logging

	finalDeadline := time.Now().Add(x.Timeout)

	if x.Retries < 0 {
		x.Retries = 0
	}
	allReqIDs := make([]uint32, 0, x.Retries+1)
	for retries := 0; ; retries++ {
		if retries > 0 {
			if LoggingDisabled != true {
				slog.Printf("Retry number %d. Last error was: %v", retries, err)
			}
			if time.Now().After(finalDeadline) {
				err = fmt.Errorf("Request timeout (after %d retries)", retries-1)
				break
			}
			if retries > x.Retries {
				// Report last error
				break
			}
		}
		err = nil

		reqDeadline := time.Now().Add(x.Timeout / time.Duration(x.Retries+1))
		x.Conn.SetDeadline(reqDeadline)

		// Request ID is an atomic counter (started at a random value)
		reqID := atomic.AddUint32(&(x.requestID), 1)
		allReqIDs = append(allReqIDs, reqID)

		var outBuf []byte
		outBuf, err = packetOut.marshalMsg(pdus, packetOut.PDUType, reqID)
		if err != nil {
			// Don't retry - not going to get any better!
			err = fmt.Errorf("marshal: %v", err)
			break
		}
		_, err = x.Conn.Write(outBuf)
		if err != nil {
			err = fmt.Errorf("Error writing to socket: %s", err.Error())
			continue
		}

		// FIXME: If our packet exceeds our buf size we'll get a partial read
		// and this request, and the next will fail. The correct logic would be
		// to realloc and read more if pack len > buff size.
		resp := make([]byte, rxBufSize, rxBufSize)
		var n int
		n, err = x.Conn.Read(resp)
		if err != nil {
			err = fmt.Errorf("Error reading from UDP: %s", err.Error())
			continue
		}

		result, err = unmarshal(resp[:n])
		if err != nil {
			err = fmt.Errorf("Unable to decode packet: %s", err.Error())
			continue
		}
		if result == nil || len(result.Variables) < 1 {
			err = fmt.Errorf("Unable to decode packet: nil")
			continue
		}

		validID := false
		for _, id := range allReqIDs {
			if id == result.RequestID {
				validID = true
			}
		}
		if !validID {
			err = fmt.Errorf("Out of order response")
			continue
		}

		// Success!
		return result, nil
	}

	// Return last error
	return nil, err
}

// Get sends an SNMP GET request
func (x *GoSNMP) Get(oids []string) (result *SnmpPacket, err error) {
	oidCount := len(oids)
	if oidCount > maxOids {
		return nil, fmt.Errorf("oid count (%d) is greater than maxOids (%d)",
			oidCount, maxOids)
	}
	// convert oids slice to pdu slice
	var pdus []SnmpPDU
	for _, oid := range oids {
		pdus = append(pdus, SnmpPDU{oid, Null, nil})
	}
	// build up SnmpPacket
	packetOut := &SnmpPacket{
		Community:  x.Community,
		Error:      0,
		ErrorIndex: 0,
		PDUType:    GetRequest,
		Version:    x.Version,
	}
	return x.send(pdus, packetOut)
}

// Set sends an SNMP SET request
func (x *GoSNMP) Set(pdus []SnmpPDU) (result *SnmpPacket, err error) {
	if len(pdus) != 1 {
		return nil, fmt.Errorf("gosnmp currently only supports SNMP SETs for one oid")
	}
	if pdus[0].Type != Integer {
		return nil, fmt.Errorf("gosnmp currently only supports SNMP SETs for Integers")
	}
	// build up SnmpPacket
	packetOut := &SnmpPacket{
		Community:  x.Community,
		Error:      0,
		ErrorIndex: 0,
		PDUType:    SetRequest,
		Version:    x.Version,
	}
	return x.send(pdus, packetOut)
}

// GetNext sends an SNMP GETNEXT request
func (x *GoSNMP) GetNext(oids []string) (result *SnmpPacket, err error) {
	oidCount := len(oids)
	if oidCount > maxOids {
		return nil, fmt.Errorf("oid count (%d) is greater than maxOids (%d)",
			oidCount, maxOids)
	}

	// convert oids slice to pdu slice
	var pdus []SnmpPDU
	for _, oid := range oids {
		pdus = append(pdus, SnmpPDU{oid, Null, nil})
	}

	// Marshal and send the packet
	packetOut := &SnmpPacket{
		Community:  x.Community,
		Error:      0,
		ErrorIndex: 0,
		PDUType:    GetNextRequest,
		Version:    x.Version,
	}

	return x.send(pdus, packetOut)
}

// GetBulk sends an SNMP GETBULK request
func (x *GoSNMP) GetBulk(oids []string, nonRepeaters uint8, maxRepetitions uint8) (result *SnmpPacket, err error) {
	oidCount := len(oids)
	if oidCount > maxOids {
		return nil, fmt.Errorf("oid count (%d) is greater than maxOids (%d)",
			oidCount, maxOids)
	}

	// convert oids slice to pdu slice
	var pdus []SnmpPDU
	for _, oid := range oids {
		pdus = append(pdus, SnmpPDU{oid, Null, nil})
	}

	// Marshal and send the packet
	packetOut := &SnmpPacket{
		Community:      x.Community,
		PDUType:        GetBulkRequest,
		Version:        x.Version,
		NonRepeaters:   nonRepeaters,
		MaxRepetitions: maxRepetitions,
	}
	return x.send(pdus, packetOut)
}

//
// SNMP Walk functions - Analogous to net-snmp's snmpwalk commands
//

// WalkFunc is the type of the function called for each data unit visited
// by the Walk function.  If an error is returned processing stops.
type WalkFunc func(dataUnit SnmpPDU) error

// BulkWalk retrieves a subtree of values using GETBULK. As the tree is
// walked walkFn is called for each new value. The function immediately returns
// an error if either there is an underlaying SNMP error (e.g. GetBulk fails),
// or if walkFn returns an error.
func (x *GoSNMP) BulkWalk(rootOid string, walkFn WalkFunc) error {
	return x.walk(GetBulkRequest, rootOid, walkFn)
}

// BulkWalkAll is similar to BulkWalk but returns a filled array of all values
// rather than using a callback function to stream results.
func (x *GoSNMP) BulkWalkAll(rootOid string) (results []SnmpPDU, err error) {
	return x.walkAll(GetBulkRequest, rootOid)
}

// Walk retrieves a subtree of values using GETNEXT - a request is made for each
// value, unlike BulkWalk which does this operation in batches. As the tree is
// walked walkFn is called for each new value. The function immediately returns
// an error if either there is an underlaying SNMP error (e.g. GetNext fails),
// or if walkFn returns an error.
func (x *GoSNMP) Walk(rootOid string, walkFn WalkFunc) error {
	return x.walk(GetNextRequest, rootOid, walkFn)
}

// WalkAll is similar to Walk but returns a filled array of all values rather
// than using a callback function to stream results.
func (x *GoSNMP) WalkAll(rootOid string) (results []SnmpPDU, err error) {
	return x.walkAll(GetNextRequest, rootOid)
}

//
// Public Functions (helpers) - in alphabetical order
//

// Partition - returns true when dividing a slice into
// partitionSize lengths, including last partition which may be smaller
// than partitionSize. This is useful when you have a large array of OIDs
// to run Get() on. See the tests for example usage.
//
// For example for a slice of 8 items to be broken into partitions of
// length 3, Partition returns true for the currentPosition having
// the following values:
//
// 0  1  2  3  4  5  6  7
//       T        T     T
//
func Partition(currentPosition, partitionSize, sliceLength int) bool {
	if currentPosition < 0 || currentPosition >= sliceLength {
		return false
	}
	if partitionSize == 1 { // redundant, but an obvious optimisation
		return true
	}
	if currentPosition%partitionSize == partitionSize-1 {
		return true
	}
	if currentPosition == sliceLength-1 {
		return true
	}
	return false
}

// ToBigInt converts SnmpPDU.Value to big.Int, or returns a zero big.Int for
// non int-like types (eg strings).
//
// This is a convenience function to make working with SnmpPDU's easier - it
// reduces the need for type assertions. A big.Int is convenient, as SNMP can
// return int32, uint32, and uint64.
func ToBigInt(value interface{}) *big.Int {
	var val int64
	switch value := value.(type) { // shadow
	case int:
		val = int64(value)
	case int8:
		val = int64(value)
	case int16:
		val = int64(value)
	case int32:
		val = int64(value)
	case int64:
		val = int64(value)
	case uint:
		val = int64(value)
	case uint8:
		val = int64(value)
	case uint16:
		val = int64(value)
	case uint32:
		val = int64(value)
	case uint64:
		return (uint64ToBigInt(value))
	case string:
		// for testing and other apps - numbers may appear as strings
		var err error
		if val, err = strconv.ParseInt(value, 10, 64); err != nil {
			return new(big.Int)
		}
	default:
		return new(big.Int)
	}
	return big.NewInt(val)
}
