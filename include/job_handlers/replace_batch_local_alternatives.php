<?php

/*
Job handler to fetch files from a local path  for batch replacement of alternative files

Requires the following job data:-
$job_data['import_path'] - Folder to scan for files to import
*/

global $lang, $baseurl_short, $offline_job_delete_completed, $fstemplate_alt_threshold;
global $notify_on_resource_change_days, $replace_alt_batch_existing;

$local_path     = $job_data['import_path'];
$minref         = $job_data['batch_replace_min'];
$maxref         = $job_data['batch_replace_max'];
$collectionid   = $job_data['batch_replace_col'];
$filename_field = $job_data['filename_field'];

if (!file_exists($local_path)) {
    job_queue_update($jobref, $job_data, STATUS_ERROR);
}

$logtext = array();

include_once __DIR__ . '/../image_processing.php';

if (!isset($collectionid) || $collectionid == 0) {
    $conditions = array();
    $minref = max((int)($minref), $fstemplate_alt_threshold);
    $firstref = max($fstemplate_alt_threshold, $minref);

    $sql_params = array("i", $minref);
    $sql_condition = "";

    if ($maxref > 0) {
        $sql_condition = " AND ref <= ?";
        $sql_params = array_merge($sql_params, array("i", (int)$maxref));
    }

    $replace_resources = ps_array("SELECT ref value FROM resource WHERE ref >= ? " . $sql_condition . " ORDER BY ref ASC", $sql_params, 0);
    $logtext[] = "Replacing files for resource IDs. Min ID: " . $minref  . (($maxref > 0) ? " Max ID: " . $maxref : "");
} else {
    $replace_resources = get_collection_resources($collectionid);
    $logtext[] = "Replacing resources within collection " . $collectionid . " only";
}

$replaced = array();
$errors = array();

$foldercontents = new DirectoryIterator($local_path);
foreach ($foldercontents as $objectindex => $object) {
    if ($object->isDot() || $object->isDir() || !($object->isReadable())) {
        continue;
    }

    $filename   = $object->getFilename();
    $extension  = $object->getExtension();
    $full_path = $local_path . DIRECTORY_SEPARATOR . $filename;

    $alternatives = get_alternative_files_by_filename($filename, $collectionid, $minref, $maxref);

    if (count($alternatives) == 0) {
        // No resource found with the same filename
        $errors[] = "Failed to find matching alternative file record for: " .  $filename;
    } else if (count($alternatives) === 1 || $replace_alt_batch_existing) {
        $copy_error = false;
        foreach ($alternatives as $alternative) {
            $ref = $alternative['ref'];
            $resource = $alternative['resource'];
            $extension = $alternative['file_extension'];
            $altfile = get_resource_path($resource, true, '', true, $extension, true, 1, false, "", $ref);
            $GLOBALS["use_error_exception"] = true;
            try {
                copy($full_path, $altfile);
                $filesize = filesize_unlimited($altfile);
                $success = filesize_unlimited($full_path) == $filesize;
            } catch (Exception $e) {
                $success = false;
                $errors[] = "ERROR - Copy operation failed for {$full_path} : " . $e->getMessage();
            }
            unset($GLOBALS["use_error_exception"]);

            if ($success) {
                ps_query(
                    "UPDATE resource_alt_files SET file_size = ? WHERE ref = ?",
                    ["i", $filesize, "i", $ref]
                );
                resource_log($resource, "u", 0);
                create_previews($resource, false, $extension, false, false, $ref);
                $replaced[$ref] = $resource;
            } else {
                $errors[] = "Failed to copy file from : " .  $full_path;
                $copy_error = true;
            }
        }
        // Attempt to delete
        if (!$copy_error) {
            try_unlink($full_path);
        }
    } else {
        // Multiple resources found with the same filename
        $resourcelist = implode(",", array_unique(array_column($alternatives, 'resource'), SORT_NUMERIC));
        $errors[] = "ERROR - multiple alternative file records found with filename '" . $filename . "'. Resource IDs : $resourcelist";
    }
}

$logtext[] = "Replaced " . count($replaced) . " alternative files: -";

if (count($replaced) > 0) {
    $logtext[] = "Replaced alternative files:";
    $logtext[] = implode(",", array_keys($replaced));
    $logtext[] = "For resource IDs:";
    $logtext[] = implode(",", array_unique(array_values($replaced), SORT_NUMERIC));
}

if (count($errors) > 0) {
    $logtext[] = "ERRORS: -";
    $logtext = array_merge($logtext, $errors);
    job_queue_update($jobref, $job_data, STATUS_ERROR);
} else {
    if ($offline_job_delete_completed) {
        job_queue_delete($jobref);
    } else {
        job_queue_update($jobref, $job_data, STATUS_COMPLETE);
    }
    $jobsuccess = true;
}

echo " --> " . implode("\n --> ", $logtext) . "\n";
if (count($replaced) > 0) {
    $url = "{$baseurl_short}pages/search.php?search=!list";
    $url .= implode(":", array_unique(array_values($replaced), SORT_NUMERIC));
} else {
    $url = "";
}
message_add($job["user"], implode("<br />", $logtext), $url);
