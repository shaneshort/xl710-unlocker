# xl710-unlocker

Make Intel X710 / XL710 / XXV710 NICs (i40e driver) accept third-party SFP+
modules, so you stop seeing:

    i40e 0000:01:00.0: Rx/Tx is disabled on this device because an unsupported SFP module type was detected.

The card refuses "unqualified" modules because of bit 11 of the *PHY
Capabilities Misc0* word, which is stored once per PHY record in the card's
NVM. `xl710-unlock` finds that table, clears the bit and fixes the NVM
checksum. Nothing has to be edited or recompiled.

> This is a Go rewrite of Wesley Terpstra's
> [terpstra/xl710-unlocker](https://github.com/terpstra/xl710-unlocker).
> The method (which NVM word to change, how to write it through the i40e
> ethtool NVM-update interface, and the checksum step) is entirely his work.
> This version adds automatic detection and safety rails around it. See
> [Credits](#credits).

## Quick start

Download the static binary from the
[latest release](https://github.com/shaneshort/xl710-unlocker/releases/latest)
on the Linux box with the card:

    curl -fLO https://github.com/shaneshort/xl710-unlocker/releases/latest/download/xl710-unlock-linux-amd64
    install -m 755 xl710-unlock-linux-amd64 ./xl710-unlock

Or build it yourself with Go 1.22+ (on any OS) and copy it over:

    make                      # -> xl710-unlock-linux-amd64
    scp xl710-unlock-linux-amd64 server:xl710-unlock

On the server:

    sudo ./xl710-unlock               # list cards, ports, firmware
    sudo ./xl710-unlock status        # find the PHY table, show locked/unlocked
    sudo ./xl710-unlock unlock -n     # dry run: show exactly what would be written
    sudo ./xl710-unlock unlock        # backup, show the change, confirm, write, verify,
                                      # then offer a global reset of the card to apply it

If there is more than one card, name one by interface (`enp1s0f0`) or PCI
address (`01:00.0`). All ports on a card share one NVM, so unlocking any port
unlocks the whole card.

Example:

    $ sudo ./xl710-unlock unlock
    enp1s0f0  0000:01:00.0  X710 10GbE SFP+ (X710-DA2/DA4)  [8086:1572]  fw 9.20 0x8000d87f 1.3353.0

    PHY capability table: 4 records at 0x6940, 14 words apart, Misc0 at +8
      record 0  Misc0 @ 0x6948 = 0x6b0c  locked (bit 11 set)
      record 1  Misc0 @ 0x6956 = 0x6b0c  locked (bit 11 set)
      record 2  Misc0 @ 0x6964 = 0x6b0c  locked (bit 11 set)
      record 3  Misc0 @ 0x6972 = 0x6b0c  locked (bit 11 set)

    Will unlock 0000:01:00 by writing:
      0x6948: 0x6b0c -> 0x630c
      ...
    and then updating the NVM checksum.
    Backup saved to xl710-nvm-0000_01_00.0-20261007-152000.bin
    Write to NVM? [y/N] y

    Done, card is unlocked in NVM.
    The card's firmware only picks this up when it restarts. That can be done now
    with a global reset of the card (all its ports drop for a few seconds).
    Reset the card now? [y/N] y
    Global reset requested; links will come back in a few seconds.

## Commands

| Command | What it does |
|---|---|
| `list` (default) | Intel 700-series cards, their ports, MACs, link state, firmware |
| `status [NIC]` | Locate the PHY table and report LOCKED / UNLOCKED / PARTIALLY UNLOCKED |
| `unlock [NIC]` | Back up the NVM, clear bit 11 in every record, update checksum, verify |
| `lock [NIC]` | Set bit 11 again (undo) |
| `reset [NIC]` | Global reset (GLOBR via i40e debugfs) so the firmware reloads its settings without a reboot |
| `checksum [NIC]` | Recompute the NVM checksum (only needed if an earlier write failed partway) |
| `backup [NIC] [-o FILE]` | Save the NVM shadow RAM (64 KiB) to a file |
| `dump [NIC] [ADDR [COUNT]]` | Hex dump NVM words, e.g. `dump 0x6940 0x40` |

Options: `-n`/`--dry-run` (show what `unlock`/`lock` would write, touch
nothing), `-y` (no confirmation), `--image FILE` (run `status`, `dump` or a
dry-run `unlock` against a backup file, on any OS), `--base ADDR` and `--misc N` (manual override
if the table isn't found automatically), `--no-backup`.

## How the table is found

Each PHY record begins with a length word, and records repeat at a stride of
length + 1. Records on one card can differ (port number, per-PHY settings),
but in every known layout the Misc0 word at +8 has `3303` two words before it
and `0a00` right after it. The tool looks for 2 to 8 consecutive records with
that shape:

| Firmware | Table start | Record | Misc0 (locked) |
|---|---|---|---|
| 5.x (original repo) | 0x6870 | `000b 0022 0083 1871 0000 0000 3303 000b 2b0c ...` | 0x2b0c |
| 6.80 | 0x693f | `000c 0022 0083 1871 0000 0000 3303 000b 6b0c 0a00 0a1e 0p03 0000` | 0x6b0c |
| 8.x / 9.x | 0x6940 / 0x6941 | `000d 0222 0083 1871 0000 0000 3303 000b 6b0c ...` | 0x6b0c |
| X722 3.33 (Dell onboard) | 0x65b6 | `000c ffc2 000a 1871 0004 0005 3303 000b 270c 0a00 ...` | already clear |

This replaces the old method of grepping a dump for `000b`/`000d` and working
out the offsets by hand
([issue #9](https://github.com/terpstra/xl710-unlocker/issues/9)). If the
tool finds nothing, or more than one candidate, it stops and tells you. Use
`dump` and `--base` from there, and please open an issue with your firmware
version and a backup.

## Applying the change

The module check is done by the card's firmware, which reads the PHY settings
from NVM when it initialises. **Reloading the i40e driver is not enough.** A
global reset of the card (`xl710-unlock reset`, the same as
`echo globr > /sys/kernel/debug/i40e/<pci>/command`) restarts it and works
without a reboot (tested on an X710-DA2, fw 6.80). Rebooting also works.

## Troubleshooting

- **Still "unsupported SFP module" after unlocking.** Check that you reset or
  rebooted; a driver reload doesn't count. After that: plain
  1G SFP (non-plus) modules aren't supported by the X710 at all, whatever
  this bit says. Some 10GBASE-T copper modules draw more power than the
  cage provides.
- **Operation not permitted.** Run it as root.
- **Device or resource busy.** The tool already retries for 15 seconds. If it
  still fails, another NVM operation is in progress; try again later.
- **A write failed partway.** The error says what was written. Re-running
  `unlock` finishes the remaining words. If only the checksum step failed,
  run `xl710-unlock checksum NIC` before rebooting.
- **Something went wrong.** `sudo ./xl710-unlock lock` reverses the change.
  Intel's `nvmupdate64e -rd` restores the NVM defaults. Updating the card to
  current Intel firmware first (with the NVM update utility) is a good idea
  anyway.

## Credits

- **Wesley W. Terpstra** worked out the unlock and wrote the original C tools
  (`mytool.c` to read the NVM, `mypoke.c` to clear the bit and update the
  checksum) in [terpstra/xl710-unlocker](https://github.com/terpstra/xl710-unlocker).
  This repository started as a fork of it; his commits are still in the history.
  The NVM access here (the ethtool `GEEPROM`/`SEEPROM` "magic" transaction
  values, the Misc0 bit, and the checksum update) is a direct re-implementation
  of his code. He described the method on the
  [e1000-devel mailing list](https://sourceforge.net/p/e1000/mailman/message/34988514/).
- **@andrewohanian** wrote the step-by-step guide in
  [terpstra/xl710-unlocker#9](https://github.com/terpstra/xl710-unlocker/issues/9),
  which documented the `000d`-based layout on firmware 8.x. The commenters
  there (including @Konata09 for 9.20 and @TheLinuxGuy) filled in other
  firmware versions and pitfalls.
- **@Nevinskas** wrote up the procedure in
  [Nevinskas/xl710-unlocker](https://github.com/Nevinskas/xl710-unlocker).

The original repository has no license. If you are Wesley, or represent him,
and want something changed here, please open an issue.
