#include <stdio.h>
int main(void) {
    unsigned w, h; if (scanf("%u %u", &w, &h) != 2) return 1;
    unsigned inside = 0;
    for (unsigned py = 0; py < h; py++)
        for (unsigned px = 0; px < w; px++) {
            double cx = (double)px / (double)w * 3.5 - 2.5, cy = (double)py / (double)h * 2.0 - 1.0;
            double x = 0.0, y = 0.0; unsigned it = 0;
            while (x * x + y * y <= 4.0 && it < 100) { double t = x * x - y * y + cx; y = 2.0 * x * y + cy; x = t; it++; }
            if (it == 100) inside++;
        }
    printf("%u\n", inside);
    return 0;
}
