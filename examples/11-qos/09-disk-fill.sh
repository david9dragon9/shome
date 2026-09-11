#!/bin/sh
# Upload more than max-disk allows.
# Expect: refused, reporting used-of-limit.
#
# Run this one directly. The quota is checked as the copy proceeds rather than
# from a declared size, so a client that lies about Content-Length gains
# nothing.
dd if=/dev/zero of=/tmp/shome-qos-big.bin bs=1m count=50 2>/dev/null
shome storage put /tmp/shome-qos-big.bin big.bin 2>&1 | tail -1
rm -f /tmp/shome-qos-big.bin
