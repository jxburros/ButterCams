# G.726 test fixtures

`src.pcm` is 0.5 s of 8 kHz mono 16-bit little-endian test signal. Each
`g726_<kbps>_<be|le>.bin` is that signal encoded by FFmpeg and each matching
`.pcm` is FFmpeg's own decoding of it, which the Go decoder must reproduce.

```
ffmpeg -f lavfi -i "aevalsrc='0.35*sin(2*PI*310*t)*(0.6+0.4*sin(2*PI*3*t))+0.2*sin(2*PI*1270*t+0.4)+0.1*sin(2*PI*(500+900*t)*t)':s=8000:d=0.5" -ac 1 -ar 8000 -f s16le src.pcm
# for br in 16 24 32 40 (code_size = br/8):
ffmpeg -f s16le -ar 8000 -ac 1 -i src.pcm -c:a g726   -b:a ${br}k -f g726   g726_${br}_be.bin
ffmpeg -f g726   -code_size $bits -ar 8000 -i g726_${br}_be.bin -f s16le g726_${br}_be.pcm
ffmpeg -f s16le -ar 8000 -ac 1 -i src.pcm -c:a g726le -b:a ${br}k -f g726le g726_${br}_le.bin
ffmpeg -f g726le -code_size $bits -ar 8000 -i g726_${br}_le.bin -f s16le g726_${br}_le.pcm
```
