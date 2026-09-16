#!/usr/bin/env python3
import mmap
import struct
import sys

with open(sys.argv[1], "rb") as source:
    with mmap.mmap(source.fileno(), 0, access=mmap.ACCESS_READ) as data:
        assert data[:6] == b"\x7fELF\x02\x01", "Expected little-endian ELF64"
        assert struct.unpack_from("<HH", data, 16) == (2, 62), "Expected x86-64 ET_EXEC"
        phoff = struct.unpack_from("<Q", data, 32)[0]
        stride, count = struct.unpack_from("<HH", data, 54)
        headers = [struct.unpack_from("<IIQQQQQQ", data, phoff + i * stride) for i in range(count)]

        def at(address, size):
            for kind, flags, offset, vaddr, _, filesz, _, _ in headers:
                if kind == 1 and vaddr <= address and address + size <= vaddr + filesz:
                    start = offset + address - vaddr
                    return data[start:start + size]
            raise AssertionError("Address is outside file-backed LOAD segments")

        identities = []
        for kind, _, offset, _, _, filesz, _, _ in headers:
            if kind != 4:
                continue
            end = offset + filesz
            while offset + 12 <= end:
                namesz, descsz, note_type = struct.unpack_from("<III", data, offset)
                offset += 12
                name = data[offset:offset + namesz]
                offset += (namesz + 3) & ~3
                desc = data[offset:offset + descsz]
                offset += (descsz + 3) & ~3
                if name == b"GNU\0" and note_type == 3:
                    identities.append(desc.hex())
        assert identities == ["3b4ce30aed886594"], identities
        assert struct.unpack("<Q", at(0x1b0a838, 8))[0] == 0x810a480, "Tick slot mismatch"
        assert at(0x810a480, 14) == bytes.fromhex("55 41 57 41 56 41 55 41 54 53 48 83 ec 48"), "Tick entry mismatch"
        assert at(0x8c52985, 6) == bytes.fromhex("ff 90 10 03 00 00"), "Engine loop dispatch mismatch"
        assert at(0x810a6cb, 5) == bytes.fromhex("e8 00 9e 1c 00"), "World tick call mismatch"
        assert at(0x810aa4c, 1) == b"\xc3", "Tick return mismatch"
        assert struct.unpack("<Q", at(0x2252418, 8))[0] == 0x9d2eab0, "Game override slot mismatch"
        assert at(0x9d2eab0, 11) == bytes.fromhex("55 41 57 41 56 41 55 41 54 53 50"), "Game override entry mismatch"
        assert at(0x9d2eb6d, 5) == bytes.fromhex("e8 0e b9 3d fe"), "Game override base call mismatch"
        assert at(0x9d2ec2c, 1) == b"\xc3", "Game override return mismatch"
        print("PASS build", identities[0], "UDomGameEngine::Tick slot 98, main-loop dispatch, nested engine/world tick, return")
