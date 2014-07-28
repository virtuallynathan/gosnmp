package gosnmp

/*
Sequence       PDUType = 0x30
GetRequest     PDUType = 0xa0
GetNextRequest PDUType = 0xa1
GetResponse    PDUType = 0xa2
SetRequest     PDUType = 0xa3
Trap           PDUType = 0xa4
GetBulkRequest PDUType = 0xa5
*/

/*
Version1  SnmpVersion = 0x0
Version2c SnmpVersion = 0x1
*/

//GenPacket generates the SNMP packet, and returns it.
func GenPacket(community string, version SnmpVersion, reqType PDUType, oids []string) ([]byte, error) {
	var packet []byte
	var pdus []SnmpPDU
	var err error
	for _, oid := range oids {
		pdus = append(pdus, SnmpPDU{oid, Null, nil})
	}

	// build up SnmpPacket
	packetOut := &SnmpPacket{
		Community:  community,
		Error:      0,
		ErrorIndex: 0,
		PDUType:    reqType,
		Version:    version,
	}

	packet, err = packetOut.marshalMsg(pdus, packetOut.PDUType, 0)
	if err != nil {
		return nil, err
	}
	return packet, nil
}
