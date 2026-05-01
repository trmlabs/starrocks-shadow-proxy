package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgproto3"
)

const (
	pgMaxStartupPacketLen = 10_000
	pgMaxMessageBodyLen   = 1 << 28
)

type pgStartupPacket struct {
	Raw     []byte
	Message pgproto3.FrontendMessage
}

type pgFrontendPacket struct {
	Raw                    []byte
	Message                pgproto3.FrontendMessage
	Command                string
	QueryText              string
	CountableQuery         bool
	ExpectsBackendResponse bool
}

type pgBackendPacket struct {
	Raw           []byte
	Message       pgproto3.BackendMessage
	Command       string
	ReadyForQuery bool
	CopyMode      bool
	AuthType      uint32
}

func readPgStartupPacket(r io.Reader) (*pgStartupPacket, error) {
	raw, err := readPgUntypedPacket(r, pgMaxStartupPacketLen)
	if err != nil {
		return nil, err
	}

	backend := pgproto3.NewBackend(bytes.NewReader(raw), io.Discard)
	msg, err := backend.ReceiveStartupMessage()
	if err != nil {
		return nil, err
	}

	return &pgStartupPacket{Raw: raw, Message: msg}, nil
}

func readPgFrontendPacket(r io.Reader, authType uint32) (*pgFrontendPacket, error) {
	raw, err := readPgTypedPacket(r, pgMaxMessageBodyLen)
	if err != nil {
		return nil, err
	}

	backend := pgproto3.NewBackend(bytes.NewReader(raw), io.Discard)
	if authType != 0 {
		if err := backend.SetAuthType(authType); err != nil {
			return nil, err
		}
	}
	msg, err := backend.Receive()
	if err != nil {
		return nil, err
	}

	return &pgFrontendPacket{
		Raw:                    raw,
		Message:                msg,
		Command:                pgFrontendCommandName(msg),
		QueryText:              pgFrontendQueryText(msg),
		CountableQuery:         pgFrontendIsCountableQuery(msg),
		ExpectsBackendResponse: pgFrontendExpectsBackendResponse(msg),
	}, nil
}

func readPgBackendPacket(r io.Reader) (*pgBackendPacket, error) {
	raw, err := readPgTypedPacket(r, pgMaxMessageBodyLen)
	if err != nil {
		return nil, err
	}

	frontend := pgproto3.NewFrontend(bytes.NewReader(raw), io.Discard)
	msg, err := frontend.Receive()
	if err != nil {
		return nil, err
	}

	packet := &pgBackendPacket{
		Raw:           raw,
		Message:       msg,
		Command:       pgBackendCommandName(msg),
		ReadyForQuery: pgBackendIsReadyForQuery(msg),
		CopyMode:      pgBackendStartsCopyMode(msg),
	}
	if auth, ok := msg.(pgproto3.AuthenticationResponseMessage); ok {
		packet.AuthType = pgAuthenticationType(auth)
	}
	return packet, nil
}

func readPgUntypedPacket(r io.Reader, maxPacketLen int) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}

	length := int(binary.BigEndian.Uint32(header))
	if length < 8 || length > maxPacketLen {
		return nil, fmt.Errorf("invalid pg startup packet length: %d", length)
	}

	raw := make([]byte, length)
	copy(raw[:4], header)
	if _, err := io.ReadFull(r, raw[4:]); err != nil {
		return nil, err
	}
	return raw, nil
}

func readPgTypedPacket(r io.Reader, maxBodyLen int) ([]byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}

	length := int(binary.BigEndian.Uint32(header[1:]))
	if length < 4 {
		return nil, fmt.Errorf("invalid pg message length: %d", length)
	}
	bodyLen := length - 4
	if bodyLen > maxBodyLen {
		return nil, fmt.Errorf("invalid pg message body length: %d exceeds max %d", bodyLen, maxBodyLen)
	}

	raw := make([]byte, 1+length)
	copy(raw[:5], header)
	if _, err := io.ReadFull(r, raw[5:]); err != nil {
		return nil, err
	}
	return raw, nil
}

func pgFrontendCommandName(msg pgproto3.FrontendMessage) string {
	switch msg.(type) {
	case *pgproto3.Bind:
		return "Bind"
	case *pgproto3.Close:
		return "Close"
	case *pgproto3.CopyData:
		return "CopyData"
	case *pgproto3.CopyDone:
		return "CopyDone"
	case *pgproto3.CopyFail:
		return "CopyFail"
	case *pgproto3.Describe:
		return "Describe"
	case *pgproto3.Execute:
		return "Execute"
	case *pgproto3.Flush:
		return "Flush"
	case *pgproto3.FunctionCall:
		return "FunctionCall"
	case *pgproto3.Parse:
		return "Parse"
	case *pgproto3.PasswordMessage, *pgproto3.SASLInitialResponse, *pgproto3.SASLResponse, *pgproto3.GSSResponse:
		return "Password"
	case *pgproto3.Query:
		return "Query"
	case *pgproto3.Sync:
		return "Sync"
	case *pgproto3.Terminate:
		return "Terminate"
	default:
		return fmt.Sprintf("%T", msg)
	}
}

func pgFrontendQueryText(msg pgproto3.FrontendMessage) string {
	switch m := msg.(type) {
	case *pgproto3.Query:
		return m.String
	case *pgproto3.Parse:
		return m.Query
	default:
		return ""
	}
}

func pgFrontendIsCountableQuery(msg pgproto3.FrontendMessage) bool {
	switch msg.(type) {
	case *pgproto3.Query, *pgproto3.Parse, *pgproto3.Execute:
		return true
	default:
		return false
	}
}

func pgFrontendExpectsBackendResponse(msg pgproto3.FrontendMessage) bool {
	switch msg.(type) {
	case *pgproto3.Query, *pgproto3.Flush, *pgproto3.Sync:
		return true
	default:
		return false
	}
}

func pgBackendCommandName(msg pgproto3.BackendMessage) string {
	switch msg.(type) {
	case *pgproto3.AuthenticationOk:
		return "AuthenticationOk"
	case *pgproto3.AuthenticationCleartextPassword:
		return "AuthenticationCleartextPassword"
	case *pgproto3.AuthenticationMD5Password:
		return "AuthenticationMD5Password"
	case *pgproto3.AuthenticationSASL:
		return "AuthenticationSASL"
	case *pgproto3.AuthenticationSASLContinue:
		return "AuthenticationSASLContinue"
	case *pgproto3.AuthenticationSASLFinal:
		return "AuthenticationSASLFinal"
	case *pgproto3.BackendKeyData:
		return "BackendKeyData"
	case *pgproto3.BindComplete:
		return "BindComplete"
	case *pgproto3.CloseComplete:
		return "CloseComplete"
	case *pgproto3.CommandComplete:
		return "CommandComplete"
	case *pgproto3.CopyBothResponse:
		return "CopyBothResponse"
	case *pgproto3.CopyData:
		return "CopyData"
	case *pgproto3.CopyDone:
		return "CopyDone"
	case *pgproto3.CopyInResponse:
		return "CopyInResponse"
	case *pgproto3.CopyOutResponse:
		return "CopyOutResponse"
	case *pgproto3.DataRow:
		return "DataRow"
	case *pgproto3.EmptyQueryResponse:
		return "EmptyQueryResponse"
	case *pgproto3.ErrorResponse:
		return "ErrorResponse"
	case *pgproto3.NoData:
		return "NoData"
	case *pgproto3.NoticeResponse:
		return "NoticeResponse"
	case *pgproto3.NotificationResponse:
		return "NotificationResponse"
	case *pgproto3.ParameterDescription:
		return "ParameterDescription"
	case *pgproto3.ParameterStatus:
		return "ParameterStatus"
	case *pgproto3.ParseComplete:
		return "ParseComplete"
	case *pgproto3.PortalSuspended:
		return "PortalSuspended"
	case *pgproto3.ReadyForQuery:
		return "ReadyForQuery"
	case *pgproto3.RowDescription:
		return "RowDescription"
	default:
		return fmt.Sprintf("%T", msg)
	}
}

func pgBackendIsReadyForQuery(msg pgproto3.BackendMessage) bool {
	_, ok := msg.(*pgproto3.ReadyForQuery)
	return ok
}

func pgBackendStartsCopyMode(msg pgproto3.BackendMessage) bool {
	switch msg.(type) {
	case *pgproto3.CopyInResponse, *pgproto3.CopyOutResponse, *pgproto3.CopyBothResponse:
		return true
	default:
		return false
	}
}

func pgAuthenticationType(msg pgproto3.AuthenticationResponseMessage) uint32 {
	switch msg.(type) {
	case *pgproto3.AuthenticationOk:
		return pgproto3.AuthTypeOk
	case *pgproto3.AuthenticationCleartextPassword:
		return pgproto3.AuthTypeCleartextPassword
	case *pgproto3.AuthenticationMD5Password:
		return pgproto3.AuthTypeMD5Password
	case *pgproto3.AuthenticationGSS:
		return pgproto3.AuthTypeGSS
	case *pgproto3.AuthenticationGSSContinue:
		return pgproto3.AuthTypeGSSCont
	case *pgproto3.AuthenticationSASL:
		return pgproto3.AuthTypeSASL
	case *pgproto3.AuthenticationSASLContinue:
		return pgproto3.AuthTypeSASLContinue
	case *pgproto3.AuthenticationSASLFinal:
		return pgproto3.AuthTypeSASLFinal
	default:
		return 0
	}
}
