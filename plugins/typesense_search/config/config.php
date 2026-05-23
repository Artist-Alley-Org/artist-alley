<?php

$typesense_search_host = '127.0.0.1';
$typesense_search_port = 8108;
$typesense_search_protocol = 'http';
$typesense_search_api_key = '';
$typesense_search_collection = 'resources';
$typesense_search_timeout = 2;
$typesense_search_global_filter=""; // String to append to the filter - will apply to all search queries. Could be set in a group override to provide group filters until search filters are supported. Example:   $typesense_search_global_filter=" && resource_type:=3" - show resources of type 3 only.

