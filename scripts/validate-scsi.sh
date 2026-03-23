python3 -c "
    import socket, struct

    # Connect
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(10)
    s.connect(('147.135.96.30', 3260))

    # Build minimal iSCSI Login Request
    bhs = bytearray(48)
    bhs[0] = 0x43  # Login Request + Immediate
    bhs[1] = 0x87  # T=1, CSG=01, NSG=11

    # Data: key-value pairs
    data = b'InitiatorName=iqn.2026-01.com.datamigrate:test\x00TargetName=datamigrate-rhel6-test-a18e594a-c92b-4850-6751-16a41c5d
    4c54\x00SessionType=Normal\x00AuthMethod=None\x00'

    # Set data segment length
    bhs[5] = (len(data) >> 16) & 0xff
    bhs[6] = (len(data) >> 8) & 0xff
    bhs[7] = len(data) & 0xff

    # ISID
    bhs[8] = 0x40
    bhs[13] = 0x01

    # ITT
    struct.pack_into('>I', bhs, 16, 1)
    # CmdSN
    struct.pack_into('>I', bhs, 24, 1)

    # Send
    s.send(bhs + data + b'\x00' * ((4 - len(data) % 4) % 4))

    # Receive response
    resp = s.recv(4096)
    opcode = resp[0] & 0x3f
    status = resp[36]
    print(f'Opcode: 0x{opcode:02x} (0x23 = Login Response)')
    print(f'Status class: {status} (0 = success)')
    if status == 0:
        print('iSCSI LOGIN SUCCESS — target is reachable and accepting connections!')
    else:
        print(f'Login failed — status class {status}, detail {resp[37]}')
        print(f'Response data: {resp[48:].decode(\"utf-8\", errors=\"ignore\")}')
    s.close()
    "