#!/bin/sh
set -eu

prefix=${PREFIX:-/usr/local}
version=${VERSION:-dev}
destination=${DESTDIR:-"$(pwd)/build/root"}
go_command=${GO:-go}

fail() {
	echo "package: $*" >&2
	exit 1
}

trusted_root_dir() {
	[ -d "$1" ] && [ ! -L "$1" ] || return 1
	dir_meta=$(LC_ALL=C /bin/ls -ldn "$1") || return 1
	set -- $dir_meta
	dir_mode=$1
	case "$dir_mode" in *[.+]) dir_mode=${dir_mode%?} ;; esac
	[ "$#" -ge 4 ] && [ "$3" = 0 ] && [ "$4" = 0 ] || return 1
	case "$dir_mode" in
		drwxrwxrwt) return 0 ;;
		d????w* | d???????w*) return 1 ;;
		d*) return 0 ;;
		*) return 1 ;;
	esac
}

private_staging_root() {
	[ -d "$1" ] && [ ! -L "$1" ] || return 1
	dir_meta=$(LC_ALL=C /bin/ls -ldn "$1") || return 1
	set -- $dir_meta
	dir_mode=$1
	case "$dir_mode" in *[.+]) dir_mode=${dir_mode%?} ;; esac
	[ "$#" -ge 4 ] && [ "$dir_mode" = drwx------ ] && [ "$3" = 0 ] && [ "$4" = 0 ]
}

trusted_broker_file() {
	[ -f "$1" ] && [ ! -L "$1" ] || return 1
	file_meta=$(LC_ALL=C /bin/ls -ldn "$1") || return 1
	set -- $file_meta
	file_mode=$1
	case "$file_mode" in *[.+]) file_mode=${file_mode%?} ;; esac
	[ "$#" -ge 4 ] && [ "$file_mode" = -rwsr-sr-x ] && [ "$3" = 0 ] && [ "$4" = 0 ]
}

trusted_root_chain() {
	chain_path=$1
	if ! trusted_root_dir /; then
		fail "broker installation ancestry is not root-owned and non-writable: /"
	fi
	chain_rest=${chain_path#/}
	chain_current=
	while [ -n "$chain_rest" ]; do
		case "$chain_rest" in
			*/*) chain_component=${chain_rest%%/*}; chain_rest=${chain_rest#*/} ;;
			*) chain_component=$chain_rest; chain_rest= ;;
		esac
		chain_current=$chain_current/$chain_component
		if ! trusted_root_dir "$chain_current"; then
			fail "broker installation ancestry is not root-owned and non-writable: $chain_current"
		fi
	done
}

# A setuid artifact is safe to construct only as root and only beneath a
# directory that an unprivileged caller cannot replace or populate. Live
# installation uses /; staged packaging requires an already-created private
# root-owned DESTDIR (mode 0700).
if [ "$(/usr/bin/id -u)" -ne 0 ]; then
	fail "root is required to establish the broker's root:root mode 6755 installation"
fi

case "$destination" in
	/*) ;;
	*) fail "DESTDIR must be absolute: $destination" ;;
esac
case "$destination" in
	/)
		destination=
		;;
	*/ | *//* | */./* | */../* | */. | */..)
		fail "DESTDIR must be lexically clean: $destination"
		;;
	*)
		if ! private_staging_root "$destination"; then
			fail "staging DESTDIR must be root-owned mode 0700: $destination"
		fi
		trusted_root_chain "$destination"
		;;
esac

case "$prefix" in
	/*) ;;
	*)
		echo "package: PREFIX must be absolute: $prefix" >&2
		exit 2
		;;
esac
case "$prefix" in
	/ | */ | *//* | */./* | */../* | */. | */..)
		echo "package: PREFIX must be lexically clean: $prefix" >&2
		exit 2
		;;
esac
case "$prefix" in
	/workspace | /workspace/* | /tmp | /tmp/* | /var/tmp | /var/tmp/*)
		echo "package: PREFIX must not select a workspace or temporary tree: $prefix" >&2
		exit 2
		;;
esac

libexec_root=$prefix/libexec/noise
module=github.com/noisefloor-oss/tasks
ldflags="-X $module/internal/buildinfo.Version=$version -X $module/internal/libexec.Root=$libexec_root"

payload=$destination$prefix/lib/noise/payload
/usr/bin/install -d -o 0 -g 0 -m 0755 \
	"$destination$prefix/bin" "$destination$prefix/lib" \
	"$destination$prefix/lib/noise" "$destination$prefix/libexec"
/usr/bin/install -d -o 0 -g 0 -m 0700 "$payload"
if ! trusted_root_dir "$destination$prefix/libexec"; then
	fail "broker installation directory is not a real root-owned non-writable directory: $destination$prefix/libexec"
fi
if [ -z "$destination" ]; then
	trusted_root_chain "$prefix/libexec"
fi
"$go_command" build -ldflags "$ldflags" -o "$destination$prefix/bin/noise" ./cmd/noise
"$go_command" build -ldflags "$ldflags" -o "$payload/noise-tasks" ./cmd/noise-tasks
"$go_command" build -ldflags "$ldflags" -o "$destination$prefix/bin/noisetask" ./cmd/noisetask
"$go_command" build -ldflags "$ldflags" -o "$destination$prefix/libexec/noise-setup" ./cmd/noise-setup
/bin/chmod 0700 "$payload" "$payload/noise-tasks"
/bin/chmod 0755 "$destination$prefix/bin/noise" "$destination$prefix/bin/noisetask" "$destination$prefix/libexec/noise-setup"
/usr/bin/install -m 0755 "$payload/noise-tasks" "$destination$prefix/bin/noise-tasks"

# §9.5 confined-registration broker, installed setuid+setgid root beneath
# the installation root's libexec directory — outside the version-swapped
# plugin tree the publisher manages, beside the capability key. The
# unprivileged minting tool for the trusted host installs next to it.
broker_path=$destination$prefix/libexec/noise-worktree-broker
broker_build=$(/usr/bin/mktemp "$destination$prefix/libexec/.noise-worktree-broker.XXXXXX")
trap '/bin/rm -f "$broker_build"' EXIT HUP INT TERM
"$go_command" build -ldflags "$ldflags" -o "$broker_build" ./cmd/noise-worktree-broker
"$go_command" build -ldflags "$ldflags" -o "$destination$prefix/libexec/noise-broker-mint" ./cmd/noise-broker-mint
/usr/bin/install -o 0 -g 0 -m 6755 "$broker_build" "$broker_path"
/bin/chmod 0755 "$destination$prefix/libexec/noise-broker-mint"
if ! trusted_broker_file "$broker_path"; then
	fail "broker installation is not a regular root:root mode-6755 file: $broker_path"
fi
/bin/rm -f "$broker_build"
trap - EXIT HUP INT TERM

echo "package: remember to install the broker capability key as root at" >&2
echo "package: $prefix/libexec/noise-worktree-broker.key (mode 0400); see docs/BROKER.md" >&2
