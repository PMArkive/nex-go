package nex

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"runtime"
	"slices"
	"time"
)

// PRUDPServer represents a bare-bones PRUDP server
type PRUDPServer struct {
	udpSocket                   *net.UDPConn
	clients                     *MutexMap[string, *PRUDPClient]
	PRUDPVersion                int
	IsQuazalMode                bool
	IsSecureServer              bool
	accessKey                   string
	kerberosPassword            []byte
	kerberosTicketVersion       int
	kerberosKeySize             int
	FragmentSize                int
	version                     *LibraryVersion
	datastoreProtocolVersion    *LibraryVersion
	matchMakingProtocolVersion  *LibraryVersion
	rankingProtocolVersion      *LibraryVersion
	ranking2ProtocolVersion     *LibraryVersion
	messagingProtocolVersion    *LibraryVersion
	utilityProtocolVersion      *LibraryVersion
	natTraversalProtocolVersion *LibraryVersion
	eventHandlers               map[string][]func(PacketInterface)
	connectionIDCounter         *Counter[uint32]
	pingTimeout                 time.Duration
}

// OnReliableData adds an event handler which is fired when a new reliable DATA packet is received
func (s *PRUDPServer) OnReliableData(handler func(PacketInterface)) {
	s.on("reliable-data", handler)
}

func (s *PRUDPServer) on(name string, handler func(PacketInterface)) {
	if _, ok := s.eventHandlers[name]; !ok {
		s.eventHandlers[name] = make([]func(PacketInterface), 0)
	}

	s.eventHandlers[name] = append(s.eventHandlers[name], handler)
}

func (s *PRUDPServer) emit(name string, packet PRUDPPacketInterface) {
	if handlers, ok := s.eventHandlers[name]; ok {
		for _, handler := range handlers {
			go handler(packet)
		}
	}
}

// Listen starts a PRUDP server on a given port
func (s *PRUDPServer) Listen(port int) {
	udpAddress, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		panic(err)
	}

	socket, err := net.ListenUDP("udp", udpAddress)
	if err != nil {
		panic(err)
	}

	s.udpSocket = socket

	quit := make(chan struct{})

	for i := 0; i < runtime.NumCPU(); i++ {
		go s.listenDatagram(quit)
	}

	<-quit
}

func (s *PRUDPServer) listenDatagram(quit chan struct{}) {
	err := error(nil)

	for err == nil {
		err = s.handleSocketMessage()
	}

	quit <- struct{}{}

	panic(err)
}

func (s *PRUDPServer) handleSocketMessage() error {
	buffer := make([]byte, 64000)

	read, addr, err := s.udpSocket.ReadFromUDP(buffer)
	if err != nil {
		return err
	}

	discriminator := addr.String()

	client, ok := s.clients.Get(discriminator)

	if !ok {
		client = NewPRUDPClient(addr, s)
		client.startHeartbeat()

		s.clients.Set(discriminator, client)
	}

	packetData := buffer[:read]
	readStream := NewStreamIn(packetData, s)

	var packets []PRUDPPacketInterface

	// * Support any packet type the client sends and respond
	// * with that same type. Also keep reading from the stream
	// * until no more data is left, to account for multiple
	// * packets being sent at once
	if bytes.Equal(packetData[:2], []byte{0xEA, 0xD0}) {
		packets, _ = NewPRUDPPacketsV1(client, readStream)
	} else {
		packets, _ = NewPRUDPPacketsV0(client, readStream)
	}

	for _, packet := range packets {
		s.processPacket(packet)
	}

	return nil
}

func (s *PRUDPServer) processPacket(packet PRUDPPacketInterface) {
	packet.Sender().(*PRUDPClient).resetHeartbeat()

	if packet.HasFlag(FlagAck) || packet.HasFlag(FlagMultiAck) {
		s.handleAcknowledgment(packet)
		return
	}

	switch packet.Type() {
	case SynPacket:
		s.handleSyn(packet)
	case ConnectPacket:
		s.handleConnect(packet)
	case DataPacket:
		s.handleData(packet)
	case DisconnectPacket:
		s.handleDisconnect(packet)
	case PingPacket:
		s.handlePing(packet)
	}
}

func (s *PRUDPServer) handleAcknowledgment(packet PRUDPPacketInterface) {
	if packet.HasFlag(FlagMultiAck) {
		s.handleMultiAcknowledgment(packet)
		return
	}

	client := packet.Sender().(*PRUDPClient)

	substream := client.reliableSubstream(packet.SubstreamID())
	substream.ResendScheduler.AcknowledgePacket(packet.SequenceID())
}

func (s *PRUDPServer) handleMultiAcknowledgment(packet PRUDPPacketInterface) {
	client := packet.Sender().(*PRUDPClient)
	stream := NewStreamIn(packet.Payload(), s)
	sequenceIDs := make([]uint16, 0)
	var baseSequenceID uint16
	var substream *ReliablePacketSubstreamManager

	if packet.SubstreamID() == 1 {
		// * New aggregate acknowledgment packets set this to 1
		// * and encode the real substream ID in in the payload
		substreamID, _ := stream.ReadUInt8()
		additionalIDsCount, _ := stream.ReadUInt8()
		baseSequenceID, _ = stream.ReadUInt16LE()
		substream = client.reliableSubstream(substreamID)

		for i := 0; i < int(additionalIDsCount); i++ {
			additionalID, _ := stream.ReadUInt16LE()
			sequenceIDs = append(sequenceIDs, additionalID)
		}
	} else {
		// TODO - This is how Kinnay's client handles this, but it doesn't make sense for QRV? Since it can have multiple reliable substreams?
		// * Old aggregate acknowledgment packets always use
		// * substream 0
		substream = client.reliableSubstream(0)
		baseSequenceID = packet.SequenceID()

		for stream.Remaining() > 0 {
			additionalID, _ := stream.ReadUInt16LE()
			sequenceIDs = append(sequenceIDs, additionalID)
		}
	}

	// * MutexMap.Each locks the mutex, can't remove while reading.
	// * Have to just loop again
	substream.ResendScheduler.packets.Each(func(sequenceID uint16, pending *PendingPacket) {
		if sequenceID <= baseSequenceID && !slices.Contains(sequenceIDs, sequenceID) {
			sequenceIDs = append(sequenceIDs, sequenceID)
		}
	})

	// * Actually remove the packets from the pool
	for _, sequenceID := range sequenceIDs {
		substream.ResendScheduler.AcknowledgePacket(sequenceID)
	}
}

func (s *PRUDPServer) handleSyn(packet PRUDPPacketInterface) {
	client := packet.Sender().(*PRUDPClient)

	var ack PRUDPPacketInterface

	if packet.Version() == 0 {
		ack, _ = NewPRUDPPacketV0(client, nil)
	} else {
		ack, _ = NewPRUDPPacketV1(client, nil)
	}

	connectionSignature, _ := packet.calculateConnectionSignature(client.address)

	client.reset()
	client.clientConnectionSignature = connectionSignature
	client.sourceStreamType = packet.SourceStreamType()
	client.sourcePort = packet.SourcePort()
	client.destinationStreamType = packet.DestinationStreamType()
	client.destinationPort = packet.DestinationPort()

	ack.SetType(SynPacket)
	ack.AddFlag(FlagAck)
	ack.AddFlag(FlagHasSize)
	ack.SetSourceStreamType(packet.DestinationStreamType())
	ack.SetSourcePort(packet.DestinationPort())
	ack.SetDestinationStreamType(packet.SourceStreamType())
	ack.SetDestinationPort(packet.SourcePort())
	ack.setConnectionSignature(connectionSignature)
	ack.setSignature(ack.calculateSignature([]byte{}, []byte{}))

	s.emit("syn", ack)

	s.sendRaw(client.address, ack.Bytes())
}

func (s *PRUDPServer) handleConnect(packet PRUDPPacketInterface) {
	client := packet.Sender().(*PRUDPClient)

	var ack PRUDPPacketInterface

	if packet.Version() == 0 {
		ack, _ = NewPRUDPPacketV0(client, nil)
	} else {
		ack, _ = NewPRUDPPacketV1(client, nil)
	}

	client.serverConnectionSignature = packet.getConnectionSignature()

	connectionSignature, _ := packet.calculateConnectionSignature(client.address)

	ack.SetType(ConnectPacket)
	ack.AddFlag(FlagAck)
	ack.AddFlag(FlagHasSize)
	ack.SetSourceStreamType(packet.DestinationStreamType())
	ack.SetSourcePort(packet.DestinationPort())
	ack.SetDestinationStreamType(packet.SourceStreamType())
	ack.SetDestinationPort(packet.SourcePort())
	ack.setConnectionSignature(make([]byte, len(connectionSignature)))
	ack.SetSessionID(0)
	ack.SetSequenceID(1)

	if ack, ok := ack.(*PRUDPPacketV1); ok {
		// * Just tell the client we support exactly what it wants
		ack.maximumSubstreamID = packet.(*PRUDPPacketV1).maximumSubstreamID
		ack.minorVersion = packet.(*PRUDPPacketV1).minorVersion
		ack.supportedFunctions = packet.(*PRUDPPacketV1).supportedFunctions

		client.minorVersion = ack.minorVersion
		client.supportedFunctions = ack.supportedFunctions
		client.createReliableSubstreams(ack.maximumSubstreamID)
	} else {
		client.createReliableSubstreams(0)
	}

	var payload []byte

	if s.IsSecureServer {
		sessionKey, pid, checkValue, err := s.readKerberosTicket(packet.Payload())
		if err != nil {
			fmt.Println(err)
		}

		client.SetPID(pid)
		client.setSessionKey(sessionKey)

		stream := NewStreamOut(s)

		// * The response value is a Buffer whose data contains
		// * checkValue+1. This is just a lazy way of encoding
		// * a Buffer type
		stream.WriteUInt32LE(4)              // * Buffer length
		stream.WriteUInt32LE(checkValue + 1) // * Buffer data

		payload = stream.Bytes()
	} else {
		payload = make([]byte, 0)
	}

	ack.SetPayload(payload)
	ack.setSignature(ack.calculateSignature([]byte{}, packet.getConnectionSignature()))

	s.emit("connect", ack)

	s.sendRaw(client.address, ack.Bytes())
}

func (s *PRUDPServer) handleData(packet PRUDPPacketInterface) {
	if packet.HasFlag(FlagReliable) {
		s.handleReliable(packet)
	} else {
		s.handleUnreliable(packet)
	}
}

func (s *PRUDPServer) handleDisconnect(packet PRUDPPacketInterface) {
	if packet.HasFlag(FlagNeedsAck) {
		s.acknowledgePacket(packet)
	}

	client := packet.Sender().(*PRUDPClient)

	client.cleanup()
	s.clients.Delete(client.address.String())

	s.emit("disconnect", packet)
}

func (s *PRUDPServer) handlePing(packet PRUDPPacketInterface) {
	if packet.HasFlag(FlagNeedsAck) {
		s.acknowledgePacket(packet)
	}
}

func (s *PRUDPServer) readKerberosTicket(payload []byte) ([]byte, uint32, uint32, error) {
	stream := NewStreamIn(payload, s)

	ticketData, err := stream.ReadBuffer()
	if err != nil {
		return nil, 0, 0, err
	}

	requestData, err := stream.ReadBuffer()
	if err != nil {
		return nil, 0, 0, err
	}

	serverKey := DeriveKerberosKey(2, s.kerberosPassword)

	ticket := NewKerberosTicketInternalData()
	err = ticket.Decrypt(NewStreamIn(ticketData, s), serverKey)
	if err != nil {
		return nil, 0, 0, err
	}

	ticketTime := ticket.Issued.Standard()
	serverTime := time.Now().UTC()

	timeLimit := ticketTime.Add(time.Minute * 2)
	if serverTime.After(timeLimit) {
		return nil, 0, 0, errors.New("Kerberos ticket expired")
	}

	sessionKey := ticket.SessionKey
	kerberos := NewKerberosEncryption(sessionKey)

	decryptedRequestData, err := kerberos.Decrypt(requestData)
	if err != nil {
		return nil, 0, 0, err
	}

	checkDataStream := NewStreamIn(decryptedRequestData, s)

	userPID, err := checkDataStream.ReadUInt32LE()
	if err != nil {
		return nil, 0, 0, err
	}

	_, err = checkDataStream.ReadUInt32LE() // * CID of secure server station url
	if err != nil {
		return nil, 0, 0, err
	}

	responseCheck, err := checkDataStream.ReadUInt32LE()
	if err != nil {
		return nil, 0, 0, err
	}

	return sessionKey, userPID, responseCheck, nil
}

func (s *PRUDPServer) acknowledgePacket(packet PRUDPPacketInterface) {
	var ack PRUDPPacketInterface

	if packet.Version() == 0 {
		ack, _ = NewPRUDPPacketV0(packet.Sender().(*PRUDPClient), nil)
	} else {
		ack, _ = NewPRUDPPacketV1(packet.Sender().(*PRUDPClient), nil)
	}

	ack.SetType(packet.Type())
	ack.AddFlag(FlagAck)
	ack.SetSourceStreamType(packet.DestinationStreamType())
	ack.SetSourcePort(packet.DestinationPort())
	ack.SetDestinationStreamType(packet.SourceStreamType())
	ack.SetDestinationPort(packet.SourcePort())
	ack.SetSequenceID(packet.SequenceID())
	ack.setFragmentID(packet.getFragmentID())
	ack.SetSubstreamID(packet.SubstreamID())

	s.sendPacket(ack)

	// * Servers send the DISCONNECT ACK 3 times
	if packet.Type() == DisconnectPacket {
		s.sendPacket(ack)
		s.sendPacket(ack)
	}
}

func (s *PRUDPServer) handleReliable(packet PRUDPPacketInterface) {
	if packet.HasFlag(FlagNeedsAck) {
		s.acknowledgePacket(packet)
	}

	substream := packet.Sender().(*PRUDPClient).reliableSubstream(packet.SubstreamID())

	for _, pendingPacket := range substream.Update(packet) {
		if packet.Type() == DataPacket {
			payload := substream.AddFragment(pendingPacket.decryptPayload())

			if packet.getFragmentID() == 0 {
				message := NewRMCMessage()
				message.FromBytes(payload)

				substream.ResetFragmentedPayload()

				packet.SetRMCMessage(message)

				s.emit("reliable-data", packet)
			}
		}
	}
}

func (s *PRUDPServer) handleUnreliable(packet PRUDPPacketInterface) {}

func (s *PRUDPServer) sendPing(client *PRUDPClient) {
	var ping PRUDPPacketInterface

	if s.PRUDPVersion == 0 {
		ping, _ = NewPRUDPPacketV0(client, nil)
	} else {
		ping, _ = NewPRUDPPacketV1(client, nil)
	}

	ping.SetType(PingPacket)
	ping.AddFlag(FlagNeedsAck)
	ping.SetSourceStreamType(client.destinationStreamType)
	ping.SetSourcePort(client.destinationPort)
	ping.SetDestinationStreamType(client.sourceStreamType)
	ping.SetDestinationPort(client.sourcePort)
	ping.SetSubstreamID(0)

	s.sendPacket(ping)
}

// Send sends the packet to the packets sender
func (s *PRUDPServer) Send(packet PacketInterface) {
	if packet, ok := packet.(PRUDPPacketInterface); ok {
		data := packet.Payload()
		fragments := int(len(data) / s.FragmentSize)

		var fragmentID uint8 = 1
		for i := 0; i <= fragments; i++ {
			if len(data) < s.FragmentSize {
				packet.SetPayload(data)
				packet.setFragmentID(0)
			} else {
				packet.SetPayload(data[:s.FragmentSize])
				packet.setFragmentID(fragmentID)

				data = data[s.FragmentSize:]
				fragmentID++
			}

			s.sendPacket(packet)
		}
	}
}

func (s *PRUDPServer) sendPacket(packet PRUDPPacketInterface) {
	client := packet.Sender().(*PRUDPClient)

	if !packet.HasFlag(FlagAck) && !packet.HasFlag(FlagMultiAck) {
		if packet.HasFlag(FlagReliable) {
			substream := client.reliableSubstream(packet.SubstreamID())
			packet.SetSequenceID(substream.NextOutgoingSequenceID())
		} else if packet.Type() == DataPacket {
			packet.SetSequenceID(client.nextOutgoingUnreliableSequenceID())
		} else if packet.Type() == PingPacket {
			packet.SetSequenceID(client.nextOutgoingPingSequenceID())
		} else {
			packet.SetSequenceID(0)
		}
	}

	if packet.Type() == DataPacket && !packet.HasFlag(FlagAck) && !packet.HasFlag(FlagMultiAck) {
		if packet.HasFlag(FlagReliable) {
			substream := client.reliableSubstream(packet.SubstreamID())
			packet.SetPayload(substream.Encrypt(packet.Payload()))
		}
		// TODO - Unreliable crypto
	}

	packet.setSignature(packet.calculateSignature(client.sessionKey, client.serverConnectionSignature))

	if packet.HasFlag(FlagReliable) && packet.HasFlag(FlagNeedsAck) {
		substream := client.reliableSubstream(packet.SubstreamID())
		substream.ResendScheduler.AddPacket(packet)
	}

	s.sendRaw(packet.Sender().Address(), packet.Bytes())
}

// sendRaw will send the given address the provided packet
func (s *PRUDPServer) sendRaw(conn net.Addr, data []byte) {
	s.udpSocket.WriteToUDP(data, conn.(*net.UDPAddr))
}

// AccessKey returns the servers sandbox access key
func (s *PRUDPServer) AccessKey() string {
	return s.accessKey
}

// SetAccessKey sets the servers sandbox access key
func (s *PRUDPServer) SetAccessKey(accessKey string) {
	s.accessKey = accessKey
}

// KerberosPassword returns the server kerberos password
func (s *PRUDPServer) KerberosPassword() []byte {
	return s.kerberosPassword
}

// SetKerberosPassword sets the server kerberos password
func (s *PRUDPServer) SetKerberosPassword(kerberosPassword []byte) {
	s.kerberosPassword = kerberosPassword
}

// SetFragmentSize sets the max size for a packets payload
func (s *PRUDPServer) SetFragmentSize(fragmentSize int) {
	// TODO - Derive this value from the MTU
	// * From the wiki:
	// *
	// * The fragment size depends on the implementation.
	// * It is generally set to the MTU minus the packet overhead.
	// *
	// * In old NEX versions, which only support PRUDP v0, the MTU is
	// * hardcoded to 1000 and the maximum payload size seems to be 962 bytes.
	// *
	// * Later, the MTU was increased to 1364, and the maximum payload
	// * size is seems to be 1300 bytes, unless PRUDP v0 is used, in which case it’s 1264 bytes.
	s.FragmentSize = fragmentSize
}

// SetKerberosTicketVersion sets the version used when handling kerberos tickets
func (s *PRUDPServer) SetKerberosTicketVersion(kerberosTicketVersion int) {
	s.kerberosTicketVersion = kerberosTicketVersion
}

// KerberosKeySize gets the size for the kerberos session key
func (s *PRUDPServer) KerberosKeySize() int {
	return s.kerberosKeySize
}

// SetKerberosKeySize sets the size for the kerberos session key
func (s *PRUDPServer) SetKerberosKeySize(kerberosKeySize int) {
	s.kerberosKeySize = kerberosKeySize
}

// LibraryVersion returns the server NEX version
func (s *PRUDPServer) LibraryVersion() *LibraryVersion {
	return s.version
}

// SetDefaultLibraryVersion sets the default NEX protocol versions
func (s *PRUDPServer) SetDefaultLibraryVersion(version *LibraryVersion) {
	s.version = version
	s.datastoreProtocolVersion = version.Copy()
	s.matchMakingProtocolVersion = version.Copy()
	s.rankingProtocolVersion = version.Copy()
	s.ranking2ProtocolVersion = version.Copy()
	s.messagingProtocolVersion = version.Copy()
	s.utilityProtocolVersion = version.Copy()
	s.natTraversalProtocolVersion = version.Copy()
}

// DataStoreProtocolVersion returns the servers DataStore protocol version
func (s *PRUDPServer) DataStoreProtocolVersion() *LibraryVersion {
	return s.datastoreProtocolVersion
}

// SetDataStoreProtocolVersion sets the servers DataStore protocol version
func (s *PRUDPServer) SetDataStoreProtocolVersion(version *LibraryVersion) {
	s.datastoreProtocolVersion = version
}

// MatchMakingProtocolVersion returns the servers MatchMaking protocol version
func (s *PRUDPServer) MatchMakingProtocolVersion() *LibraryVersion {
	return s.matchMakingProtocolVersion
}

// SetMatchMakingProtocolVersion sets the servers MatchMaking protocol version
func (s *PRUDPServer) SetMatchMakingProtocolVersion(version *LibraryVersion) {
	s.matchMakingProtocolVersion = version
}

// RankingProtocolVersion returns the servers Ranking protocol version
func (s *PRUDPServer) RankingProtocolVersion() *LibraryVersion {
	return s.rankingProtocolVersion
}

// SetRankingProtocolVersion sets the servers Ranking protocol version
func (s *PRUDPServer) SetRankingProtocolVersion(version *LibraryVersion) {
	s.rankingProtocolVersion = version
}

// Ranking2ProtocolVersion returns the servers Ranking2 protocol version
func (s *PRUDPServer) Ranking2ProtocolVersion() *LibraryVersion {
	return s.ranking2ProtocolVersion
}

// SetRanking2ProtocolVersion sets the servers Ranking2 protocol version
func (s *PRUDPServer) SetRanking2ProtocolVersion(version *LibraryVersion) {
	s.ranking2ProtocolVersion = version
}

// MessagingProtocolVersion returns the servers Messaging protocol version
func (s *PRUDPServer) MessagingProtocolVersion() *LibraryVersion {
	return s.messagingProtocolVersion
}

// SetMessagingProtocolVersion sets the servers Messaging protocol version
func (s *PRUDPServer) SetMessagingProtocolVersion(version *LibraryVersion) {
	s.messagingProtocolVersion = version
}

// UtilityProtocolVersion returns the servers Utility protocol version
func (s *PRUDPServer) UtilityProtocolVersion() *LibraryVersion {
	return s.utilityProtocolVersion
}

// SetUtilityProtocolVersion sets the servers Utility protocol version
func (s *PRUDPServer) SetUtilityProtocolVersion(version *LibraryVersion) {
	s.utilityProtocolVersion = version
}

// SetNATTraversalProtocolVersion sets the servers NAT Traversal protocol version
func (s *PRUDPServer) SetNATTraversalProtocolVersion(version *LibraryVersion) {
	s.natTraversalProtocolVersion = version
}

// NATTraversalProtocolVersion returns the servers NAT Traversal protocol version
func (s *PRUDPServer) NATTraversalProtocolVersion() *LibraryVersion {
	return s.natTraversalProtocolVersion
}

// ConnectionIDCounter returns the servers CID counter
func (s *PRUDPServer) ConnectionIDCounter() *Counter[uint32] {
	return s.connectionIDCounter
}

// NewPRUDPServer will return a new PRUDP server
func NewPRUDPServer() *PRUDPServer {
	return &PRUDPServer{
		clients:             NewMutexMap[string, *PRUDPClient](),
		IsQuazalMode:        false,
		kerberosKeySize:     32,
		FragmentSize:        1300,
		eventHandlers:       make(map[string][]func(PacketInterface)),
		connectionIDCounter: NewCounter[uint32](10),
		pingTimeout:         time.Second * 15,
	}
}