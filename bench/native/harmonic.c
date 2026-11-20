#include <stdio.h>
int main(void) {
    unsigned n; if (scanf("%u", &n) != 1) return 1;
    double s = 0.0;
    for (unsigned i = 1; i < n; i++) { double x = (double)i; s += 1.0 / (x * x); }
    printf("%.6f\n", s);
    return 0;
}
