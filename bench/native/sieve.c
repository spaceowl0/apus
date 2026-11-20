#include <stdio.h>
#include <string.h>
int main(void) {
    unsigned n; if (scanf("%u", &n) != 1) return 1;
    static unsigned char sieve[100000]; memset(sieve, 1, sizeof sieve);
    sieve[0] = sieve[1] = 0;
    for (unsigned i = 2; i * i < n; i++)
        if (sieve[i]) for (unsigned j = i * i; j < n; j += i) sieve[j] = 0;
    unsigned count = 0;
    for (unsigned k = 2; k < n; k++) if (sieve[k]) count++;
    printf("%u\n", count);
    return 0;
}
