#include <stdio.h>
static unsigned steps(unsigned long n) {
    unsigned s = 0;
    while (n != 1) { n = (n % 2 == 0) ? n / 2 : 3 * n + 1; s++; }
    return s;
}
int main(void) {
    unsigned long limit; if (scanf("%lu", &limit) != 1) return 1;
    unsigned best = 0; unsigned long arg = 1;
    for (unsigned long n = 1; n < limit; n++) { unsigned s = steps(n); if (s > best) { best = s; arg = n; } }
    printf("%lu %u\n", arg, best);
    return 0;
}
