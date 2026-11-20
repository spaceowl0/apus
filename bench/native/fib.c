#include <stdio.h>
#include <stdlib.h>
static unsigned long fib(unsigned n) { return n < 2 ? n : fib(n - 1) + fib(n - 2); }
int main(int argc, char **argv) {
    unsigned n = argc > 1 ? (unsigned)atoi(argv[1]) : 42;   /* runtime value: cannot be folded */
    printf("%lu\n", fib(n));
    return 0;
}
