# perfSONAR: ps-splunk
Splunk perfSONAR tools

## Installation
* Define the SPLUNK_HOME directory
```shell
export SPLUNK_HOME=/opt/splunk
```
* Clone the `ps-splunk` application
```shell
git clone git@github.com:bored-engineer/ps-splunk.git $SPLUNK_HOME/etc/apps/ps-splunk
```
* (Re)start Splunk so that the app is recognized.
```shell
$SPLUNK_HOME/bin/splunk restart
```
