package guest

// BootKernelFunc is the name of the shell function BootKernelShell defines.
const BootKernelFunc = "boot_kernel"

// BootKernelShell defines boot_kernel, which prints the version of the kernel
// GRUB boots by default from the boot directory boot, or an empty line when
// there is none to read. It runs under dash as the jail's run user (doctor)
// and as root (the bring-up), so it only reads files every user can.
//
// /boot/vmlinuz is not the answer: the kernel package's postinst points it at
// the kernel installed last, so a GA kernel update after the HWE kernel moves
// it to the older kernel, while GRUB still boots the newest.
//
//   - grub.cfg, when its only default is entry 0 (Ubuntu's GRUB_DEFAULT=0):
//     the first menuentry's linux line. grub-mkconfig leaves the file
//     world-readable unless it holds a password. ${next_entry} is a one-shot
//     grub-reboot choice, which lever never sets.
//   - Otherwise (no grub.cfg, unreadable, another default, or a linux line
//     without a vmlinuz-<version>): the newest /boot/vmlinuz-* by version
//     sort, the kernel Ubuntu's 10_linux puts first.
//
// Only the version's own characters are printed, since callers show it.
func BootKernelShell(boot string) string {
	q := shellSingleQuote
	return `boot_kernel() {
  bk_cfg=` + q(boot+"/grub/grub.cfg") + `
  bk=
  if [ -r "$bk_cfg" ] && [ "$(sed -n 's/^[[:space:]]*set default="\(.*\)"[[:space:]]*$/\1/p' "$bk_cfg" | grep -vF '${next_entry}' | sort -u)" = 0 ]; then
    bk=$(awk '/^[[:space:]]*menuentry[[:space:]]/ { m = 1; next } m && $1 == "linux" { print $2; exit }' "$bk_cfg")
    case "$bk" in */vmlinuz-*) bk=${bk##*/vmlinuz-} ;; *) bk= ;; esac
  fi
  if [ -z "$bk" ]; then
    bk=$(ls -1 ` + q(boot) + ` 2>/dev/null | sed -n 's/^vmlinuz-//p' | sort -V | tail -n 1)
  fi
  case "$bk" in *[!A-Za-z0-9._+~-]*) bk= ;; esac
  printf '%s\n' "$bk"
}
`
}
