#/bin/bash

# canonicalize OSTYPE to OSKIND
case $OSTYPE in
darwin*)        OSKIND=darwin ;;
linux*)         OSKIND=linux ;;
openbsd*)       OSKIND=openbsd ;;
freebsd*)       OSKIND=freebsd ;;
*)              OSKIND=$OSTYPE ;;
esac

# canonicalize CPU to ARCH
[ -z "$CPU" ] && CPU=`uname -m`
case $CPU in
amd64|x86_64|x86-64)    ARCH=x86_64 ;;
i386|x86)               ARCH=x86 ;;
*)                      ARCH=$CPU
esac

which mpv > /dev/null 2>&1
if [[ $? -ne 0 ]]; then	
	echo "This script requires mpv to play the stream." >&2
	exit 1
fi

ICYCAT="./bin/$OSKIND.$ARCH/icycat"
if [[ ! -x $ICYCAT ]]; then
	ICYCAT="./icycat"

	if [[ ! -x $ICYCAT ]]; then
		echo "you need to build icycat first!" >&2
		exit 1
	fi
fi

exec $ICYCAT --logtostderr --stderrthreshold=INFO http://cdn.nrjaudio.fm/adwz1/de/33001/mp3_128.mp3 | mpv /dev/stdin
