// @decision(DL-056)

/** The mark's geometry, shared by Logo.svelte and public/favicon.svg.
 *
 *  One path with three subpaths: the outline, then the two enclosed
 *  cutouts. They are holes rather than white shapes drawn on top, so
 *  the interior is genuinely transparent and the mark reads correctly
 *  on either theme — with `fill-rule="evenodd"`, which every consumer
 *  must set. */
export const MarkPath =
  'M 578.1 212.5 C 661 213.8 731.6 248 774.9 319.2 C 812.3 380.7 819.6 382.9 887 405.8 C 907.1 412.6 927.2 419.5 947.3 426.6 C 1027.9 455.2 1107 489.9 1178.8 537.3 C 1247 582.2 1361.3 681 1378.1 764.9 C 1356.2 752 1333.2 728.2 1313.9 710.7 C 1270.5 670.9 1222.9 635.9 1172.1 606.2 C 1071.6 547.8 918.4 495.6 802.6 501.3 C 757.3 503.6 704.8 519.7 674.1 554.5 C 622.9 612.5 663.7 667.4 697.1 720 C 715.6 749 728.5 771.5 742.3 802.8 C 768.6 862.3 778.1 909.9 781.9 973.8 C 783.4 1153.2 708.1 1290.1 561.7 1394.7 C 551.2 1402.3 540 1409.3 529.6 1417 C 535.6 1401.5 537.3 1386.8 540.1 1370.5 C 549.8 1314.7 552.2 1257.7 537.8 1202.2 C 517.6 1124.8 470.8 1060.3 402.1 1019.1 C 318.9 969.1 200.4 961.7 107.6 986.1 C 101.9 965.7 99 949.6 95.4 928.9 C 168.5 910 257.5 898.4 312.1 841.4 C 376.7 776.2 349.6 698.6 330.3 621 C 302.5 508.7 307.5 379.6 392.3 291.6 C 443.4 238.6 504.6 214.2 578.1 212.5 z M 379.2 440.5 C 382.6 443.7 389.9 474 393.4 482 C 428.6 562.7 508.2 600.5 593.4 585.8 C 585.1 677.7 620.8 704.1 664.8 779.5 C 759.3 941.4 738.8 1131.2 614.4 1271.2 L 607.1 1278.3 C 600.3 1190.3 584.2 1126 529.3 1053.9 C 485.6 996.7 424.4 956.3 356 934.8 C 339.1 929.6 321.2 924.5 303.5 922.3 C 311.5 916.2 321.1 910.4 329.3 904.4 C 353.7 886.4 372.8 863.9 387.2 837.5 C 438.5 743.1 392.4 653.3 377.6 557.6 C 371.3 516.8 372.6 481.1 379.2 440.5 z M 575.5 271.4 C 629.2 268.1 685.8 295.3 717.2 338.7 C 729.8 356.1 740.1 373.5 753.9 390.5 C 727.5 386.4 689.7 381.7 668.5 364.8 C 650.7 349.7 635.9 325.5 608.8 330.7 C 569.9 338 561.2 382.3 591.8 406 C 613.1 422.5 637.7 406.8 660.7 409.5 C 682.4 412.1 703.8 423.1 722.1 434.8 C 729.5 439.9 733.2 444.4 738.7 451.5 C 724.4 456.2 704.2 462.6 691.3 469.7 C 648.1 493.8 614.8 527.1 562.6 530.2 C 494.7 534.3 441.9 483.8 438.1 416.7 C 436 379.8 443.9 345.5 469.3 317.2 C 498.7 284.4 533.1 273.7 575.5 271.4 z';

/** The mark's own bounds, with a little air. Trimmed from the supplied
 *  artwork's 1532x1632 canvas, 38% of which was empty margin. */
export const MarkViewBox = '83 200 1307 1228';

/** Slightly wider than tall. A square box would squash it. */
export const MarkAspect = 1307 / 1228;

/*  The favicon's two colour literals deliberately do NOT live here.
 *  public/favicon.svg cannot inherit currentColor — a favicon is
 *  fetched outside the page — so it carries copies of `--ink`, and
 *  logo.test.ts checks them against src/styles/global.css itself.
 *  A copy checked against a second copy passes while both drift. */
