#/bin/sh

exec ./icycat --logtostderr --stderrthreshold=INFO http://cdn.nrjaudio.fm/adwz1/de/33001/mp3_128.mp3 | mpv /dev/stdin
