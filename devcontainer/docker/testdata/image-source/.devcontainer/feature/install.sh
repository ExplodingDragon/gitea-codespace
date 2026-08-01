#!/bin/sh
set -eu
mkdir -p /opt/local-feature/bin
printf '#!/bin/sh\nprintf local-feature\n' > /opt/local-feature/bin/local-feature
chmod +x /opt/local-feature/bin/local-feature
