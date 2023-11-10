package main

import (
	"crypto/rand"

	"github.com/PretendoNetwork/nex-go"
)

func generateTicket(userPID uint32, targetPID uint32) []byte {
	userKey := nex.DeriveKerberosKey(userPID, []byte("abcdefghijklmnop"))
	targetKey := nex.DeriveKerberosKey(targetPID, []byte("password"))
	sessionKey := make([]byte, authServer.KerberosKeySize())

	rand.Read(sessionKey)

	ticketInternalData := nex.NewKerberosTicketInternalData()
	serverTime := nex.NewDateTime(0)
	serverTime.UTC()

	ticketInternalData.Issued = serverTime
	ticketInternalData.SourcePID = userPID
	ticketInternalData.SessionKey = sessionKey

	encryptedTicketInternalData, _ := ticketInternalData.Encrypt(targetKey, nex.NewStreamOut(authServer))

	ticket := nex.NewKerberosTicket()
	ticket.SessionKey = sessionKey
	ticket.TargetPID = targetPID
	ticket.InternalData = encryptedTicketInternalData

	encryptedTicket, _ := ticket.Encrypt(userKey, nex.NewStreamOut(authServer))

	return encryptedTicket
}