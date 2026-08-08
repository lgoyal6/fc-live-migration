# Renders the blackout distribution from a bench CSV to a PNG histogram.
#   gnuplot -c histogram.gnuplot bench-results/blackout.csv out.png
#
# CSV columns: run,source,blackout_ms,agent_blackout_ms,brownout_ms,...
set datafile separator ","
csv = ARG1
out = ARG2

set terminal pngcairo size 900,520 font "sans,12"
set output out
set title "Firecracker live-migration blackout (agent pause→resume, N migrations)"
set xlabel "blackout (ms)"
set ylabel "count"
set grid ytics
set xrange [0:35]   # keep the 30 ms budget line in frame
set style fill solid 0.7 border -1
set boxwidth 0.9 relative

binwidth = 1.0
bin(x) = binwidth * floor(x / binwidth) + binwidth / 2.0

# 30ms budget line.
set arrow from 30, graph 0 to 30, graph 1 nohead lc rgb "#c0392b" lw 2 dt 2
set label "30 ms budget" at 30, graph 0.95 right rotate by 90 tc rgb "#c0392b" offset -1,0

# Column 4 is agent_blackout_ms (the true stop-the-world downtime); column 3
# (client blackout) carries this environment's jitter floor. Plot the agent
# figure — the honest one — against the budget.
plot csv using (bin($4)):(1.0) smooth freq with boxes lc rgb "#2c7fb8" notitle
