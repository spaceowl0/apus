#include <stdio.h>
#include <string.h>
static unsigned table[256];
int main(void) {
    char line[1001]; unsigned reps;
    if (!fgets(line, sizeof line, stdin)) return 1;
    size_t n = strlen(line); if (n && line[n - 1] == '\n') line[--n] = 0;
    if (scanf("%u", &reps) != 1) return 1;
    for (unsigned i = 0; i < 256; i++) { unsigned c = i; for (int k = 0; k < 8; k++) c = (c & 1) ? 0xEDB88320u ^ (c >> 1) : c >> 1; table[i] = c; }
    unsigned long total = 0;
    for (unsigned r = 0; r < reps; r++) {
        unsigned crc = 0xFFFFFFFFu;
        for (size_t i = 0; i < n; i++) crc = table[(crc ^ (unsigned char)line[i]) & 255] ^ (crc >> 8);
        total += crc ^ 0xFFFFFFFFu;
    }
    printf("%lu\n", total);
    return 0;
}
