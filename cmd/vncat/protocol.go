package main

const (
	encodingRaw         = 0
	encodingCopyRect    = 1
	encodingHextile     = 5
	encodingDesktopSize = ^uint32(222)
	encodingLastRect    = ^uint32(223)
)

const (
	updatePixelMultiplier  = 4
	opaqueAlpha            = 255
	hextileMask            = 31
	nibbleBits             = 4
	nibbleMask             = 15
	latin1Maximum          = 255
	unicodeKeysymMask      = 0x01000000
	pointerButtonMask      = 7
	pointerMessageSize     = 6
	clientKeyMessage       = 4
	clientPointerMessage   = 5
	usageExitStatus        = 2
	connectSeconds         = 10
	inputQueueCapacity     = 16
	refreshFrequency       = 30
	capabilitySeconds      = 2
	graphicsQueryID        = 31
	handshakeSeconds       = 30
	securityVNC            = 2
	protocolMinorLegacy    = 3
	writeSeconds           = 3
	colorMapCapacity       = 65536
	colorMapEntryBytes     = 6
	serverBellMessage      = 2
	serverClipboardMessage = 3
	protocolMinorModern    = 8
	passwordCapacity       = 64
	controlCancel          = 3
	controlBackspace       = 8
	passwordLimit          = 1024
	sequenceLimit          = 4096
	alternateSeparators    = 2
	controlEscape          = 27
	escapePrefixSize       = 2
	legacyMouseSize        = 6
	stringControlOverhead  = 4
	controlBell            = 7
	oscBellOverhead        = 3
)

const (
	controlQuit   = 29
	controlDelete = 127
	pixelBytes    = 4
)
