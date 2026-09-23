#!/bin/sh
# Hidden acceptance: run the agent's transform on input it never saw.
[ -f transform.sh ] || { echo "transform.sh missing"; exit 1; }
sh transform.sh hidden.csv > out.txt 2>&1 || { cat out.txt; exit 1; }
diff -u expected.txt out.txt
