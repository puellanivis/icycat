#/bin/bash

# canonicalize OSTYPE to OSKIND
case $OSTYPE in
darwin*)  OSKIND=darwin ;;
linux*)   OSKIND=linux ;;
openbsd*) OSKIND=openbsd ;;
freebsd*) OSKIND=freebsd ;;
cygwin*)  OSKIND=windows ;;
*)        OSKIND=$OSTYPE ;;
esac

# canonicalize CPU to ARCH
[ -z "$CPU" ] && CPU=`uname -m`
case $CPU in
arm64|amd64|x86_64|x86-64) ARCH=x86_64 ;;
i386|x86)            ARCH=x86 ;;
*)                   ARCH=$CPU ;;
esac

PLAY=""
OUTPUT=""
METRICS=""
while [[ $# -gt 0 ]]; do
	key="$1"
	val="${key#*=}"

	case $key in
	--play)
		PLAY="mpv"
	;;
	--play=*)
		PLAY="$val"
	;;
	--stream)
		OUTPUT="udp://127.0.0.1:1234?pkt_size=1316&tos=0x80&ttl=5"
	;;
	--output=*)
		OUTPUT="$val"
	;;
	--metrics)
		METRICS="--metrics"
	;;

	--)
		shift
		break
	;;
	--*)
		echo "unknow flag $1" >&2
		exit 1
	;;
	*)
		break
	;;
	esac
	shift
done

if [[ -n $PLAY && -n $OUTPUT ]]; then
	echo "Cannot specify both --play and an --output/--stream at the same time" >&2
	exit 1
fi

if [[ $PLAY != "" && $PLAY = "mpv" ]]; then
	FOUND="$(which mpv 2> /dev/null)"
	if [[ $? -ne 0 ]]; then	
		echo "Executable $PLAY not found." >&2
		exit 1
	fi
	PLAY="$FOUND -"
fi

ICYCAT="./bin/$OSKIND.$ARCH/icycat"
if [[ ! -x $ICYCAT ]]; then
	ICYCAT="./icycat"

	if [[ ! -x $ICYCAT ]]; then
		echo "you need to build icycat first!" >&2
		exit 1
	fi
fi

if [[ $OUTPUT != "" ]]; then
	OUTPUT="--output $OUTPUT"
fi

URL="https://frontend.streamonkey.net/energy-digital/stream/mp3?aggregator=energyde"
if [[ -n $PLAY ]]; then
	$ICYCAT --logtostderr --stderrthreshold=INFO $METRICS $OUTPUT $URL | $PLAY
	exit $?
fi

exec $ICYCAT --logtostderr --stderrthreshold=INFO $METRICS $OUTPUT --timeout=10s $URL
